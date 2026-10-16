package worker

import (
	"sync"
)

// Task represents a unit of work to be processed by the worker pool.
type Task func()

// Pool manages a pool of worker goroutines.
type Pool struct {
	tasks    chan Task
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewPool creates a new Pool with a specified number of workers.
func NewPool(numWorkers int) *Pool {
	pool := &Pool{
		tasks: make(chan Task, max(numWorkers*4, 16)),
	}

	for range numWorkers {
		pool.wg.Add(1)
		go pool.worker()
	}

	return pool
}

// worker is the function that each worker goroutine runs.
func (p *Pool) worker() {
	defer p.wg.Done()
	for task := range p.tasks {
		if task != nil {
			task()
		}
	}
}

// Submit adds a task to the worker pool.
func (p *Pool) Submit(task Task) {
	p.tasks <- task
}

// Stop stops all the workers after draining submitted tasks and waits for them to finish.
func (p *Pool) Stop() {
	p.stopOnce.Do(func() {
		close(p.tasks)
	})
	p.wg.Wait()
}
