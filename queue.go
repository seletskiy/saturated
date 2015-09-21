package main

import (
	"sync"
	"sync/atomic"
)

type BuildQueue struct {
	*sync.Mutex
	clients map[string]*CountedMutex
}

type CountedMutex struct {
	sync.Mutex

	locked int64
}

func (mutex *CountedMutex) Lock() {
	atomic.AddInt64(&mutex.locked, +1)
	mutex.Mutex.Lock()
}

func (mutex *CountedMutex) Unlock() {
	mutex.Mutex.Unlock()
	atomic.AddInt64(&mutex.locked, -1)
}

func NewBuildQueue() *BuildQueue {
	return &BuildQueue{
		Mutex:   &sync.Mutex{},
		clients: make(map[string]*CountedMutex),
	}
}

func (queue *BuildQueue) Seize(id string) {
	queue.Lock()

	if queue.clients[id] == nil {
		queue.clients[id] = &CountedMutex{}
	}

	queue.Unlock()

	queue.clients[id].Lock()
}

func (queue *BuildQueue) Free(id string) {
	queue.Lock()

	mutex := queue.clients[id]

	defer queue.Unlock()

	if queue.clients[id].locked == 0 {
		delete(queue.clients, id)
	}

	mutex.Unlock()
}

func (queue *BuildQueue) GetSize(id string) int64 {
	queue.Lock()
	defer queue.Unlock()

	if queue.clients[id] == nil {
		return 0
	}

	return queue.clients[id].locked
}
