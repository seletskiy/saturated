package main

import (
	"container/ring"
	"fmt"
	"io"
	"io/ioutil"
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

type HTTPHandler struct {
	reposPath      string
	listenAddress  string
	buildUid       int
	buildCommand   string
	installCommand string
	branchName     string
	queue          *BuildQueue

	lastBuild      *ring.Ring
	buildListMutex sync.Mutex
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
}

func (info BuildInfo) Duration() time.Duration {
	switch info.finishedAt {
	case time.Time{}:
		return time.Now().Sub(info.startedAt)
	default:
		return info.finishedAt.Sub(info.startedAt)
	}
}

const apiSummary = `
    * /v1/build/<repo-url>

      - GET: clone specified repo, build package and run install command;
        output logs in realtime.

    * /v1/key/

      - GET: get current user RSA public key.`

const usage = `saturated - dead simple makepkg build server.

Tool will serve REST API on specified address:

` + apiSummary + `

Usage:
    saturated [options] <address>
    saturated -h | --help
    saturated -v | --version

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
    -k <count>    Maximum builds count to keep in ring buffer.
                  [default: 20]
    -u <user>     Run build command with privileges of specified user.
                  [default: nobody]
    -h --help     Show this help.
    -v --version  Show version.
`

func main() {
	args, err := docopt.Parse(usage, nil, true, "3.1", false)
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

	err = checkSeteuid(buildUid)
	if err != nil {
		log.Fatal(err)
	}

	handler := &HTTPHandler{
		reposPath:      reposPath,
		listenAddress:  listenAddress,
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

// checkSeteuid calls syscall SYS_SETREUID with new uid and tries to restore
// original euid
func checkSeteuid(uid int) (err error) {
	olduid := syscall.Getuid()

	// be aware, err is named returning variable and changes by reference
	err = rawSeteuid(uid)
	if err != nil {
		return fmt.Errorf("can't setuid to %d: %s", uid, err)
	}

	err = rawSeteuid(olduid)
	if err != nil {
		return fmt.Errorf("can't restore uid to %d: %s", olduid, err)
	}

	return nil
}

func (handler *HTTPHandler) ServeHTTP(
	response http.ResponseWriter, request *http.Request,
) {
	url := request.URL.Path

	switch {
	case strings.TrimSuffix(url, "/") == "/v1/builds":
		handler.serveRequestListBuilds(response, request)

	case strings.HasPrefix(url, "/v1/build/"):
		handler.serveRequestBuild(response, request)

	case strings.TrimSuffix(url, "/") == "/v1/key":
		handler.serveRequestKey(response, request)

	case url == "/":
		handler.serveRoot(response, request)

	default:
		http.NotFound(response, request)
	}
}
func (handler *HTTPHandler) serveRequestBuild(
	response http.ResponseWriter, request *http.Request,
) {
	repoURL := strings.TrimPrefix(request.URL.Path, "/v1/build/")

	if repoURL == "" {
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

	queueSize := handler.queue.GetSize(repoURL)
	if queueSize > 0 {
		fmt.Fprintf(
			topLevelLogger, "you are %d in the build queue", queueSize,
		)

	}

	handler.queue.Seize(repoURL)

	defer handler.queue.Free(repoURL)

	fmt.Fprintf(topLevelLogger, "running build task for '%s'", repoURL)

	err := request.ParseForm()
	if err != nil {
		response.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(topLevelLogger, "error parsing request: %s", err)
		return
	}

	environ := request.Form["environ"]

	buildInfo := &BuildInfo{
		repository: repoURL,
		startedAt:  time.Now(),
		status:     "in progress",
	}

	handler.saveNewBuild(buildInfo)

	runtime.LockOSThread()

	fmt.Fprintf(topLevelLogger, "changing uid to %d", handler.buildUid)

	err = rawSeteuid(handler.buildUid)
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(
			topLevelLogger, "can't set uid to %d: %s", handler.buildUid, err,
		)
		return
	}

	err = runBuild(
		repoURL,
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

		response.WriteHeader(http.StatusBadRequest)
	} else {
		fmt.Fprintf(topLevelLogger, "build completed")
		buildInfo.status = "success"
	}
}

func (handler *HTTPHandler) serveRequestListBuilds(
	response http.ResponseWriter, request *http.Request,
) {
	writer := tabwriter.NewWriter(response, 20, 8, 4, ' ', 0)
	defer writer.Flush()

	fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
		"Repo URL", "Duration", "Status", "Error Message",
	)

	handler.lastBuild.Do(func(val interface{}) {
		if val == nil {
			return
		}

		buildInfo := val.(*BuildInfo)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			buildInfo.repository, buildInfo.Duration(),
			buildInfo.status, buildInfo.error,
		)

	})
}

func (handler *HTTPHandler) serveRequestKey(
	response http.ResponseWriter, request *http.Request,
) {
	keyPath := filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub")

	keyData, err := ioutil.ReadFile(keyPath)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}

	response.Write(keyData)
}

func (handler *HTTPHandler) serveRoot(
	response http.ResponseWriter, request *http.Request,
) {
	_, err := fmt.Fprintln(response, apiSummary)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
}

func runBuild(
	repoURL, reposPath, branchName, buildCommand, installCommand string,
	logger PrefixLogger, environ []string,
) error {
	repoDir := regexp.MustCompile(`[^\w-@.]`).ReplaceAllLiteralString(
		repoURL, "__",
	)

	repoPath := filepath.Join(reposPath, repoDir)

	workDir := repoPath + "%work"

	task := &Task{
		logger:  logger,
		workDir: workDir,
	}

	err := task.updateMirror(repoURL, repoPath)
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

func (handler *HTTPHandler) saveNewBuild(buildInfo *BuildInfo) {
	handler.buildListMutex.Lock()
	defer handler.buildListMutex.Unlock()

	// moving backward for LIFO order in list
	handler.lastBuild = handler.lastBuild.Prev()
	handler.lastBuild.Value = buildInfo
}

func rawSeteuid(uid int) error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_SETREUID, uintptr(uid), 0, 0)
	if errno != 0 {
		return errno
	}

	return nil
}
