package main

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

type PrefixLogger struct {
	output io.WriteCloser
	prefix string
}

type NilCloser struct {
	io.Writer
}

func (closer NilCloser) Close() error {
	return nil
}

func (logger PrefixLogger) Write(data []byte) (int, error) {
	prefixedData := regexp.MustCompile(`(?m)^`).ReplaceAllLiteral(
		bytes.TrimRight(data, "\n"),
		[]byte(logger.prefix),
	)

	_, err := logger.output.Write(prefixedData)
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (logger PrefixLogger) WithPrefix(prefix string) PrefixLogger {
	logger.prefix = prefix
	return logger
}

func (logger PrefixLogger) Close() error {
	return logger.output.Close()
}

type LineFlushLogger struct {
	mutex   *sync.Mutex
	output  io.Writer
	flusher http.Flusher
	buffer  bytes.Buffer
}

func NewLineFlushLogger(
	flusher http.Flusher, output io.Writer,
) LineFlushLogger {
	return LineFlushLogger{
		output:  output,
		flusher: flusher,
		mutex:   &sync.Mutex{},
	}
}

func (logger LineFlushLogger) Write(data []byte) (int, error) {
	_, err := logger.buffer.Write(data)
	if err != nil {
		return 0, err
	}

	err = logger.Flush()
	if err != nil {
		return 0, err
	}

	return len(data), nil
}

func (logger LineFlushLogger) Flush() error {
	logger.mutex.Lock()
	defer logger.mutex.Unlock()

	scanner := bufio.NewScanner(&logger.buffer)
	for scanner.Scan() {
		line := scanner.Bytes()

		_, err := logger.output.Write([]byte(string(line) + "\n"))
		if err != nil {
			return err
		}

		logger.flusher.Flush()
	}

	return nil
}

func (logger LineFlushLogger) Close() error {
	return logger.Flush()
}

type ConsoleLog struct{}

func (logger ConsoleLog) Write(data []byte) (int, error) {
	log.Println(strings.TrimRight(string(data), "\n"))

	return len(data), nil
}
