package main

import "sync"

// runPool runs check for every task on `concurrency` workers and calls emit
// for each result from a single goroutine, in completion order. A slow task
// only holds up its own worker.
func runPool(tasks []task, concurrency int, check func(task) int, emit func(task, int)) {
	type result struct {
		t      task
		status int
	}
	taskCh := make(chan task)
	resultCh := make(chan result, concurrency)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				resultCh <- result{t, check(t)}
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	for r := range resultCh {
		emit(r.t, r.status)
	}
}
