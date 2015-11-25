package main

import (
	"container/ring"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/docopt/docopt-go"
)

type BuildHandler struct {
	reposPath      string
	listenAddress  string
	buildUid       int
	buildCommand   string
	installCommand string
	branchName     string
	queue          *BuildQueue

	lastBuild *ring.Ring
}

type RepoExistError struct {
	error
}

type BuildInfo struct {
	repository string
	startedAt  time.Time
	finishedAt time.Time
	status     string
	error      string

	pointer *ring.Ring
}

func (info BuildInfo) Save() {
	info.pointer.Value = info
}

func (info BuildInfo) Duration() time.Duration {
	switch info.finishedAt {
	case time.Time{}:
		return time.Now().Sub(info.startedAt)
	default:
		return info.finishedAt.Sub(info.startedAt)
	}
}

const usage = `saturated - dead simple makepkg build server.

Tool will serve REST API on specified address:

    * /v1/build/<repo-url>

      - GET: clone specified repo, build package and run install command;
           output logs in realtime.

Usage:
    $0 [options] <address>
    $0 -h | --help
    $0 -v | --version

Options:
    -m <build>    Build command.
                  [default: makepkg -sr --noconfirm]
    -i <install>  Install command.
                  [default: /usr/lib/saturated/install-package]
    -w <workdir>  Workdir to use for cloning and building packages.
                  [default: /tmp/]
    -b <branch>   Branch, that will be used for checkout. This branch should
                  contain PKGBUILD file.
                  [default: pkgbuild]
    -h --help     Show this help.
    -v --version  Show version.
    -u <user>     Run build command with privileges of specified user.
                  [default: nobody]
    -k <count>    Maximum builds count to keep in ring buffer.
                  [default: 20]
`

func main() {
	args, err := docopt.Parse(
		strings.Replace(usage, "$0", filepath.Base(os.Args[0]), -1),
		nil, true, "1.0", false,
	)
	if err != nil {
		panic(err)
	}

	var (
		reposPath           = args["-w"].(string)
		listenAddress       = args["<address>"].(string)
		buildUserName       = args["-u"].(string)
		buildCommand        = args["-m"].(string)
		installCommand      = args["-i"].(string)
		branchName          = args["-b"].(string)
		maxBuildCountString = args["-k"].(string)
	)
	maxBuildCount, err := strconv.Atoi(maxBuildCountString)
	if err != nil {
		log.Fatalf("can't parse max builds count: %s", err)
	}

	buildUser, err := user.Lookup(buildUserName)
	if err != nil {
		log.Fatalf("can't lookup user '%s': %s", buildUserName, err)
	}

	buildUid, _ := strconv.Atoi(buildUser.Uid)

	err = checkSetuid(buildUid)
	if err != nil {
		log.Fatalf("setuid %d check failed: %s", buildUid, err)
	}

	handler := &BuildHandler{
		reposPath:      reposPath,
		listenAddress:  listenAddress,
		buildUid:       buildUid,
		buildCommand:   buildCommand,
		installCommand: installCommand,
		branchName:     branchName,
		queue:          NewBuildQueue(),

		lastBuild: ring.New(maxBuildCount),
	}

	log.Printf("listening on '%s'...", listenAddress)

	err = http.ListenAndServe(listenAddress, handler)
	if err != nil {
		log.Fatalf("can't listen on '%s': %s", listenAddress, err)
	}
}

// checkSetuid calls syscall SYS_SETUID in safe mode (in additional goroutine)
func checkSetuid(uid int) (err error) {
	waiting := sync.WaitGroup{}
	waiting.Add(1)

	go func() {
		defer waiting.Done()

		// be aware, err is named returning variable and changes by reference
		err = rawSetuid(uid)
	}()

	waiting.Wait()

	return err
}

func (handler *BuildHandler) ServeHTTP(
	response http.ResponseWriter, request *http.Request,
) {
	switch {
	case strings.TrimSuffix(request.URL.Path, "/") == "/v1/builds":
		handler.handleListBuilds(response, request)
	case strings.HasPrefix(request.URL.Path, "/v1/build/"):
		handler.handleBuild(response, request)
	default:
		http.NotFound(response, request)
	}
}

func (handler *BuildHandler) handleBuild(
	response http.ResponseWriter, request *http.Request,
) {
	repoUrl := strings.TrimPrefix(request.URL.Path, "/v1/build/")
	if repoUrl == "" {
		response.WriteHeader(http.StatusBadRequest)
		io.WriteString(
			response,
			"error: <repo-url> required in URL /v1/build/<repo-url>",
		)
		return
	}

	logger := PrefixLogger{
		output: NewLineFlushLogger(
			response.(http.Flusher),
			io.MultiWriter(
				PrefixLogger{
					prefix: fmt.Sprintf("(%s) ", request.RemoteAddr),
					output: NilCloser{ConsoleLog{}},
				},
				response,
			),
		),
	}

	topLevelLogger := logger.WithPrefix("* ")

	queueSize := handler.queue.GetSize(repoUrl)
	if queueSize > 0 {
		fmt.Fprintf(
			topLevelLogger, "you are %d in the build queue", queueSize,
		)

	}

	handler.queue.Seize(repoUrl)

	defer handler.queue.Free(repoUrl)

	fmt.Fprintf(topLevelLogger, "running build task for '%s'", repoUrl)

	err := request.ParseForm()
	if err != nil {
		response.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(topLevelLogger, "error parsing request: %s", err)
		return
	}

	environ := request.Form["environ"]

	runtime.LockOSThread()

	err = rawSetuid(handler.buildUid)
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(
			topLevelLogger, "can't set uid to %d: %s", handler.buildUid, err,
		)
		return
	}

	buildInfo := handler.saveNewBuild(repoUrl)
	err = runBuild(
		repoUrl,
		handler.reposPath,
		handler.branchName,
		handler.buildCommand,
		handler.installCommand,
		logger,
		environ,
	)
	buildInfo.finishedAt = time.Now()
	if err != nil {
		fmt.Fprintf(topLevelLogger, "error during build: %s", err)
		buildInfo.status = "error"
		buildInfo.error = err.Error()
	} else {
		fmt.Fprintf(topLevelLogger, "build completed")
		buildInfo.status = "success"
	}
	buildInfo.Save()
}

func (handler BuildHandler) handleListBuilds(
	response http.ResponseWriter, request *http.Request,
) {
	writer := tabwriter.NewWriter(response, 20, 8, 4, ' ', 0)
	defer writer.Flush()

	fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
		"Repo url", "Duration", "Status", "Error Message",
	)

	handler.lastBuild.Next().Do(func(val interface{}) {
		if val == nil {
			return
		}

		buildInfo := val.(BuildInfo)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			buildInfo.repository, buildInfo.Duration(),
			buildInfo.status, buildInfo.error,
		)

	})
}

func runBuild(
	repoUrl, reposPath, branchName, buildCommand, installCommand string,
	logger PrefixLogger, environ []string,
) error {
	repoDir := regexp.MustCompile(`[^\w-@.]`).ReplaceAllLiteralString(
		repoUrl, "__",
	)

	repoPath := filepath.Join(reposPath, repoDir)

	workDir := repoPath + "%work"

	task := &Task{
		logger:  logger,
		workDir: workDir,
	}

	err := task.updateMirror(repoUrl, repoPath)
	if err != nil {
		return fmt.Errorf("can't update mirror: %s", err)
	}

	err = task.run(
		repoPath, branchName, buildCommand, installCommand, environ,
	)
	if err != nil {
		return fmt.Errorf("can't install package: %s", err)
	}

	return nil
}

func rawSetuid(uid int) error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_SETUID, uintptr(uid), 0, 0)
	if errno != 0 {
		return errno
	}

	return nil
}

func (handler *BuildHandler) saveNewBuild(repoName string) BuildInfo {
	info := BuildInfo{
		repository: repoName,
		startedAt:  time.Now(),
		status:     "in progress",

		pointer: handler.lastBuild,
	}
	handler.lastBuild.Value = info

	handler.lastBuild = handler.lastBuild.Prev()
	return info
}
