package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docopt/docopt-go"
)

type BuildHandler struct {
	reposPath      string
	listenAddress  string
	buildCommand   string
	installCommand string
	branchName     string
	queue          *BuildQueue
}

type RepoExistError struct {
	error
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
		reposPath      = args["-w"].(string)
		listenAddress  = args["<address>"].(string)
		buildCommand   = args["-m"].(string)
		installCommand = args["-i"].(string)
		branchName     = args["-b"].(string)
	)

	handler := &BuildHandler{
		reposPath:      reposPath,
		listenAddress:  listenAddress,
		buildCommand:   buildCommand,
		installCommand: installCommand,
		branchName:     branchName,
		queue:          NewBuildQueue(),
	}

	log.Printf("listening on '%s'...", listenAddress)

	err = http.ListenAndServe(listenAddress, handler)
	if err != nil {
		log.Fatalf("can't listen on '%s': %s", listenAddress, err)
	}
}

func (handler *BuildHandler) ServeHTTP(
	response http.ResponseWriter, request *http.Request,
) {
	if !strings.HasPrefix(request.URL.Path, "/v1/build/") {
		response.WriteHeader(http.StatusNotFound)
		return
	}

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
	err = runBuild(
		repoUrl,
		handler.reposPath,
		handler.branchName,
		handler.buildCommand,
		handler.installCommand,
		logger,
		environ,
	)
	if err != nil {
		fmt.Fprintf(topLevelLogger, "error during build: %s", err)
	} else {
		fmt.Fprintf(topLevelLogger, "build completed")
	}
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
