// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package retry

import (
	"time"

	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"go.uber.org/zap"
)

// backoffStrategy represents enum of retry wait interval.
type backoffStrategy uint8

const (
	// Stable represents fixed time wait retry policy, every retry should wait a fixed time.
	Stable backoffStrategy = iota + 1
	// LinearIncrease represents increase time wait retry policy, every retry should wait more time depends on increasing retry times.
	LinearIncrease
)

// Params define parameters for Apply
// it's a parameters union set of all implements which implement Apply.
type Params struct {
	RetryCount         int
	FirstRetryDuration time.Duration

	BackoffStrategy backoffStrategy

	// IsRetryableFn tells whether we should retry when operateFn failed
	// params: (number of retry, error of operation)
	// return: (bool)
	//   1. true: means operateFn can be retried
	//   2. false: means operateFn cannot retry after receive this error
	IsRetryableFn func(int, error) bool
}

func NewParams(retryCount int, firstRetryDuration time.Duration, backoffStrategy backoffStrategy,
	isRetryableFn func(int, error) bool,
) *Params {
	return &Params{
		RetryCount:         retryCount,
		FirstRetryDuration: firstRetryDuration,
		BackoffStrategy:    backoffStrategy,
		IsRetryableFn:      isRetryableFn,
	}
}

// OperateFunc the function we can retry
//
//	return: (result of operation, error of operation)
type OperateFunc func(*tcontext.Context) (interface{}, error)

// Strategy define different kind of retry strategy.
type Strategy interface {
	// Apply define retry strategy
	// params: (retry parameters for this strategy, a normal operation)
	// return: (result of operation, number of retry, error of operation)
	Apply(ctx *tcontext.Context,
		params Params,
		operateFn OperateFunc,
	) (interface{}, int, error)
}

// FiniteRetryStrategy will retry `RetryCount` times when failed to operate DB.
type FiniteRetryStrategy struct{}

// Apply for FiniteRetryStrategy, it waits `FirstRetryDuration` before it starts first retry, and then rest of retries wait time depends on BackoffStrategy.
func (*FiniteRetryStrategy) Apply(ctx *tcontext.Context, params Params, operateFn OperateFunc,
) (ret interface{}, i int, err error) {
	for ; i < params.RetryCount; i++ {
		ret, err = operateFn(ctx)
		if err != nil {
			if params.IsRetryableFn(i, err) {
				duration := params.FirstRetryDuration

				switch params.BackoffStrategy {
				case LinearIncrease:
					duration = time.Duration(i+1) * params.FirstRetryDuration
				default:
				}
				log.L().Warn("retry strategy takes effect", zap.Error(err), zap.Int("retry_times", i), zap.Int("retry_count", params.RetryCount))

				select {
				case <-ctx.Context().Done():
					log.L().Warn("Context Done happened before retry backoff", zap.Int("retry_times", i), zap.Int("retry_count", params.RetryCount))
					return ret, i, err // return `ret` rather than `nil`
				case <-time.After(duration):
				}
				continue
			}
			log.L().Warn("error is not retryable", zap.Error(err), zap.Int("retry_times", i), zap.Int("retry_count", params.RetryCount))
		}
		break
	}
	return ret, i, err
}

// Retryer retries operateFn until success or reaches retry limit
// todo: merge with Strategy when refactor
type Retryer interface {
	Apply(ctx *tcontext.Context, operateFn OperateFunc) (interface{}, int, error)
}

// FiniteRetryer wraps params.
type FiniteRetryer struct {
	FiniteRetryStrategy
	Params *Params
}

func (s *FiniteRetryer) Apply(ctx *tcontext.Context, operateFn OperateFunc) (ret interface{}, i int, err error) {
	return s.FiniteRetryStrategy.Apply(ctx, *s.Params, operateFn)
}
