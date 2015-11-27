package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

type Task struct {
	logger  PrefixLogger
	workDir string
}

func (task *Task) updateMirror(repoURL, repoPath string) error {
	err := task.clone(repoURL, repoPath)
	if err != nil {
		if _, ok := err.(RepoExistError); !ok {
			return err
		}
	}

	err = task.fetch(repoPath)
	if err != nil {
		return err
	}

	return nil
}

func (task *Task) run(
	repoPath, branchName, buildCommand, installCommand string,
	environ []string,
) error {
	defer task.cleanWorkDir()

	err := task.createWorkDir(repoPath)
	if err != nil {
		return err
	}

	err = task.checkoutBranch(branchName)
	if err != nil {
		return fmt.Errorf("can't checkout branch '%s': %s", branchName, err)
	}

	err = task.buildPackage(buildCommand, environ)
	if err != nil {
		return fmt.Errorf("can't build package: %s", err)
	}

	if installCommand != "" {
		err = task.installPackage(installCommand)
		if err != nil {
			return err
		}
	}

	return nil
}

func (task *Task) clone(repoURL, repoPath string) error {
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err == nil {
		return RepoExistError{}
	}

	return runCommandWithLog(
		exec.Command("git", "clone", "--mirror", repoURL, repoPath),
		task.logger.WithPrefix("[clone] "),
	)
}

func (task *Task) fetch(repoPath string) error {
	cmd := exec.Command("git", "fetch", "-pt", "--all")
	cmd.Dir = repoPath
	return runCommandWithLog(
		cmd,
		task.logger.WithPrefix("[fetch] "),
	)
}

func (task *Task) createWorkDir(source string) error {
	return runCommandWithLog(
		exec.Command("git", "clone", source, task.workDir),
		task.logger.WithPrefix("[workdir] "),
	)
}

func (task *Task) checkoutBranch(branch string) error {
	return runCommandWithLog(
		exec.Command("git", "-C", task.workDir, "checkout", branch),
		task.logger.WithPrefix("[workdir] "),
	)
}

func (task *Task) cleanWorkDir() error {
	fmt.Fprintf(
		task.logger.WithPrefix("[clean] "),
		"working dir '%s' cleared", task.workDir,
	)

	return os.RemoveAll(task.workDir)
}

func (task *Task) buildPackage(commandString string, environ []string) error {
	command := makeShellCommand(commandString, task.workDir)
	command.Env = append(environ, os.Environ()...)
	return runCommandWithLog(command, task.logger.WithPrefix("[build] "))
}

func (task *Task) installPackage(command string) error {
	return runCommandWithLog(
		makeShellCommand(command, task.workDir),
		task.logger.WithPrefix("[install] "),
	)
}

func runCommandWithLog(command *exec.Cmd, logger io.WriteCloser) error {
	command.Stdout = logger
	command.Stderr = logger

	err := command.Run()
	if err != nil {
		return err
	}

	return logger.Close()
}

func makeShellCommand(
	command string, workDir string, args ...string,
) *exec.Cmd {
	cmd := exec.Command("sh", append([]string{"-c", command}, args...)...)
	cmd.Dir = workDir
	return cmd
}
