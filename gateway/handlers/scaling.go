// Copyright (c) OpenFaaS Author(s). All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package handlers

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/openfaas/faas/gateway/scaling"
)

// ScalingConfig for scaling behaviours
type ScalingConfig struct {
	// MaxPollCount attempts to query a function before giving up
	MaxPollCount uint

	// FunctionPollInterval delay or interval between polling a function's readiness status
	FunctionPollInterval time.Duration

	// CacheExpiry life-time for a cache entry before considering invalid
	CacheExpiry time.Duration

	// ServiceQuery queries available/ready replicas for function
	ServiceQuery scaling.ServiceQuery
}

// NewFunctionScaler create a new scaler with the specified
// ScalingConfig
func NewFunctionScaler(config ScalingConfig) FunctionScaler {
	cache := scaling.FunctionCache{
		Cache:  make(map[string]*scaling.FunctionMeta),
		Expiry: config.CacheExpiry,
	}

	return FunctionScaler{
		Cache:  &cache,
		Config: config,
	}
}

// FunctionScaler scales from zero
type FunctionScaler struct {
	Cache  *scaling.FunctionCache
	Config ScalingConfig
}

// FunctionScaleResult holds the result of scaling from zero
type FunctionScaleResult struct {
	Available bool
	Error     error
	Found     bool
	Duration  time.Duration
}

// Scale scales a function from zero replicas to 1 or the value set in
// the minimum replicas metadata
func (f *FunctionScaler) Scale(functionName string) FunctionScaleResult {
	start := time.Now()
	queryResponse, err := f.Config.ServiceQuery.GetReplicas(functionName)

	if err != nil {
		return FunctionScaleResult{
			Error:     err,
			Available: false,
			Found:     false,
			Duration:  time.Since(start),
		}
	}

	f.Cache.Set(functionName, queryResponse)

	if queryResponse.AvailableReplicas == 0 {
		minReplicas := uint64(1)
		if queryResponse.MinReplicas > 0 {
			minReplicas = queryResponse.MinReplicas
		}

		log.Printf("[Scale] function=%s 0 => %d requested", functionName, minReplicas)

		setScaleErr := f.Config.ServiceQuery.SetReplicas(functionName, minReplicas)
		if setScaleErr != nil {
			return FunctionScaleResult{
				Error:     fmt.Errorf("unable to scale function [%s], err: %s", functionName, err),
				Available: false,
				Found:     true,
				Duration:  time.Since(start),
			}
		}

		for i := 0; i < int(f.Config.MaxPollCount); i++ {
			queryResponse, err := f.Config.ServiceQuery.GetReplicas(functionName)
			f.Cache.Set(functionName, queryResponse)
			totalTime := time.Since(start)

			if err != nil {
				return FunctionScaleResult{
					Error:     err,
					Available: false,
					Found:     true,
					Duration:  totalTime,
				}
			}

			if queryResponse.AvailableReplicas > 0 {

				log.Printf("[Scale] function=%s 0 => %d successful - %f seconds", functionName, queryResponse.AvailableReplicas, totalTime.Seconds())

				return FunctionScaleResult{
					Error:     nil,
					Available: true,
					Found:     true,
					Duration:  totalTime,
				}
			}

			time.Sleep(f.Config.FunctionPollInterval)
		}
	}

	return FunctionScaleResult{
		Error:     nil,
		Available: true,
		Found:     true,
		Duration:  time.Since(start),
	}
}

// MakeScalingHandler creates handler which can scale a function from
// zero to N replica(s). After scaling the next http.HandlerFunc will
// be called. If the function is not ready after the configured
// amount of attempts / queries then next will not be invoked and a status
// will be returned to the client.
func MakeScalingHandler(next http.HandlerFunc, config ScalingConfig) http.HandlerFunc {

	scaler := NewFunctionScaler(config)

	return func(w http.ResponseWriter, r *http.Request) {

		functionName := getServiceName(r.URL.String())
		res := scaler.Scale(functionName)

		if !res.Found {
			errStr := fmt.Sprintf("error finding function %s: %s", functionName, res.Error.Error())
			log.Printf("Scaling: %s", errStr)

			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(errStr))
			return
		}

		if res.Error != nil {
			errStr := fmt.Sprintf("error finding function %s: %s", functionName, res.Error.Error())
			log.Printf("Scaling: %s", errStr)

			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errStr))
			return
		}

		if res.Available {
			next.ServeHTTP(w, r)
			return
		}

		log.Printf("[Scale] function=%s 0=>N timed-out after %f seconds", functionName, res.Duration.Seconds())
	}
}
