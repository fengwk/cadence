// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package task

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
)

type (
	// WeightedRoundRobinTaskSchedulerOptions configs WRR task scheduler
	WeightedRoundRobinTaskSchedulerOptions struct {
		Weights     []int
		QueueSize   int
		WorkerCount int
		RetryPolicy backoff.RetryPolicy
	}

	weightedRoundRobinTaskSchedulerImpl struct {
		status       int32
		numQueues    int
		taskChs      []chan PriorityTask
		shutdownCh   chan struct{}
		notifyCh     chan struct{}
		dispatcherWG sync.WaitGroup
		logger       log.Logger
		metricsScope metrics.Scope
		options      *WeightedRoundRobinTaskSchedulerOptions

		processor Processor
	}
)

const (
	wRRTaskProcessorQueueSize = 1
)

var (
	// ErrTaskSchedulerClosed is the error returned when submitting task to a stopped scheduler
	ErrTaskSchedulerClosed = errors.New("task scheduler has already shutdown")
)

// NewWeightedRoundRobinTaskScheduler creates a new WRR task scheduler
func NewWeightedRoundRobinTaskScheduler(
	logger log.Logger,
	metricsScope metrics.Scope,
	options *WeightedRoundRobinTaskSchedulerOptions,
) (Scheduler, error) {
	if len(options.Weights) == 0 {
		return nil, errors.New("weight is not specified in the scheduler option")
	}

	numPriorities := len(options.Weights)
	scheduler := &weightedRoundRobinTaskSchedulerImpl{
		status:       common.DaemonStatusInitialized,
		numQueues:    numPriorities,
		taskChs:      make([]chan PriorityTask, numPriorities),
		shutdownCh:   make(chan struct{}),
		notifyCh:     make(chan struct{}, 1),
		logger:       logger,
		metricsScope: metricsScope,
		options:      options,
		processor: NewParallelTaskProcessor(
			logger,
			metricsScope,
			&ParallelTaskProcessorOptions{
				QueueSize:   wRRTaskProcessorQueueSize,
				WorkerCount: options.WorkerCount,
				RetryPolicy: options.RetryPolicy,
			},
		),
	}

	for i := 0; i < numPriorities; i++ {
		scheduler.taskChs[i] = make(chan PriorityTask, options.QueueSize)
	}

	return scheduler, nil
}

func (w *weightedRoundRobinTaskSchedulerImpl) Start() {
	if !atomic.CompareAndSwapInt32(&w.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	w.processor.Start()

	w.dispatcherWG.Add(1)
	go w.dispatcher()

	w.logger.Info("Weighted round robin task scheduler started.")
}

func (w *weightedRoundRobinTaskSchedulerImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&w.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	close(w.shutdownCh)

	w.processor.Stop()

	if success := common.AwaitWaitGroup(&w.dispatcherWG, time.Minute); !success {
		w.logger.Warn("Weighted round robin task scheduler timedout on shutdown.")
	}

	w.logger.Info("Weighted round robin task scheduler shutdown.")
}

func (w *weightedRoundRobinTaskSchedulerImpl) Submit(task PriorityTask) error {
	w.metricsScope.IncCounter(metrics.PriorityTaskSubmitRequest)
	sw := w.metricsScope.StartTimer(metrics.PriorityTaskSubmitLatency)
	defer sw.Stop()

	priority := task.Priority()
	if priority >= w.numQueues {
		return errors.New("task priority exceeds limit")
	}
	select {
	case w.taskChs[priority] <- task:
		select {
		case w.notifyCh <- struct{}{}:
			// sent a notification to the dispatcher
		default:
			// do not block if there's already a notification
		}
		return nil
	case <-w.shutdownCh:
		return ErrTaskSchedulerClosed
	}
}

func (w *weightedRoundRobinTaskSchedulerImpl) dispatcher() {
	defer w.dispatcherWG.Done()

	outstandingTasks := false

	for {
		if !outstandingTasks {
			// if no task is dispatched in the last round,
			// wait for a notification
			select {
			case <-w.notifyCh:
				// block until there's a new task
			case <-w.shutdownCh:
				return
			}
		}

		outstandingTasks = false
		for priority := 0; priority < w.numQueues; priority++ {
			for i := 0; i < w.options.Weights[priority]; i++ {
				select {
				case task := <-w.taskChs[priority]:
					// dispatched at least one task in this round
					outstandingTasks = true

					if err := w.processor.Submit(task); err != nil {
						w.logger.Error("Fail to submit task to processor", tag.Error(err))
						task.Nack()
					}
				case <-w.shutdownCh:
					return
				default:
					// if no task, don't block. Skip to next priority
					break
				}
			}
		}
	}
}