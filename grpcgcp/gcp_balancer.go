/*
 *
 * Copyright 2018 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpcgcp

import (
	"google.golang.org/grpc/balancer"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/resolver"
)

const (
	// Name is the name of grpc_gcp balancer.
	Name = "grpc_gcp"

	// Default settings for max pool size and max concurrent streams.
	defaultMaxConn   = 10
	defaultMaxStream = 100

	healthCheckEnabled = true
)

func init() {
	balancer.Register(newBuilder())
}

type Config struct {
	HealthCheck bool
}

type gcpBalancerBuilder struct {
	name string
}

// currBalancer keeps the reference for the currently used balancer, only for testings.
var currBalancer *gcpBalancer

func (bb *gcpBalancerBuilder) Build(
	cc balancer.ClientConn,
	opt balancer.BuildOptions,
) balancer.Balancer {
	currBalancer = &gcpBalancer{
		cc:          cc,
		affinityMap: make(map[string]*subConnRef),
		scRefs:      make(map[balancer.SubConn]*subConnRef),
		csEvltr:     &connectivityStateEvaluator{},
		// Initialize picker to a picker that always return
		// ErrNoSubConnAvailable, because when state of a SubConn changes, we
		// may call UpdateBalancerState with this picker.
		picker: NewErrPicker(balancer.ErrNoSubConnAvailable),
	}
	return currBalancer
}

func (*gcpBalancerBuilder) Name() string {
	return Name
}

// newBuilder creates a new grpcgcp balancer builder.
func newBuilder() balancer.Builder {
	return &gcpBalancerBuilder{
		name: Name,
	}
}

// connectivityStateEvaluator gets updated by addrConns when their
// states transition, based on which it evaluates the state of
// ClientConn.
type connectivityStateEvaluator struct {
	numReady            uint64 // Number of addrConns in ready state.
	numConnecting       uint64 // Number of addrConns in connecting state.
	numTransientFailure uint64 // Number of addrConns in transientFailure.
}

// recordTransition records state change happening in every subConn and based on
// that it evaluates what aggregated state should be.
// It can only transition between Ready, Connecting and TransientFailure. Other states,
// Idle and Shutdown are transitioned into by ClientConn; in the beginning of the connection
// before any subConn is created ClientConn is in idle state. In the end when ClientConn
// closes it is in Shutdown state.
//
// recordTransition should only be called synchronously from the same goroutine.
func (cse *connectivityStateEvaluator) recordTransition(
	oldState,
	newState connectivity.State,
) connectivity.State {
	// Update counters.
	for idx, state := range []connectivity.State{oldState, newState} {
		updateVal := 2*uint64(idx) - 1 // -1 for oldState and +1 for new.
		switch state {
		case connectivity.Ready:
			cse.numReady += updateVal
		case connectivity.Connecting:
			cse.numConnecting += updateVal
		case connectivity.TransientFailure:
			cse.numTransientFailure += updateVal
		}
	}

	// Evaluate.
	if cse.numReady > 0 {
		return connectivity.Ready
	}
	if cse.numConnecting > 0 {
		return connectivity.Connecting
	}
	return connectivity.TransientFailure
}

// subConnRef keeps reference to the real SubConn with its
// connectivity state, affinity count and streams count.
type subConnRef struct {
	subConn     balancer.SubConn
	scState     connectivity.State
	affinityCnt uint32 // Keeps track of the number of keys bound to the subConn
	streamsCnt  uint32 // Keeps track of the number of streams opened on the subConn
}

type gcpBalancer struct {
	addrs   []resolver.Address
	cc      balancer.ClientConn
	csEvltr *connectivityStateEvaluator
	state   connectivity.State
	// Maps affinity key to subConnRef object
	affinityMap map[string]*subConnRef
	// Maps SubConn to its subConnRef
	scRefs map[balancer.SubConn]*subConnRef
	picker balancer.Picker
}

func (gb *gcpBalancer) HandleResolvedAddrs(addrs []resolver.Address, err error) {
	if err != nil {
		grpclog.Infof(
			"grpcgcp.gcpBalancer: HandleResolvedAddrs called with error %v",
			err,
		)
		return
	}
	grpclog.Infoln("grpcgcp.gcpBalancer: got new resolved addresses: ", addrs)
	gb.addrs = addrs

	if len(gb.scRefs) == 0 {
		gb.newSubConn()
		return
	}

	for _, scRef := range gb.scRefs {
		// TODO(weiranf): update streams count when new addrs resolved?
		scRef.subConn.UpdateAddresses(addrs)
		scRef.subConn.Connect()
	}
}

// newSubConn creates a new SubConn using cc.NewSubConn and initialize the subConnRef.
func (gb *gcpBalancer) newSubConn() {
	sc, err := gb.cc.NewSubConn(
		gb.addrs,
		balancer.NewSubConnOptions{HealthCheckEnabled: healthCheckEnabled},
	)
	if err != nil {
		grpclog.Errorf("grpcgcp.gcpBalancer: failed to NewSubConn: %v", err)
		return
	}
	gb.scRefs[sc] = &subConnRef{
		subConn:     sc,
		scState:     connectivity.Idle,
		streamsCnt:  0,
		affinityCnt: 0,
	}
	sc.Connect()
}

// bindSubConn binds the given affinity key to an existing subConnRef.
func (gb *gcpBalancer) bindSubConn(bindKey string, scRef *subConnRef) {
	_, ok := gb.affinityMap[bindKey]
	if !ok {
		gb.affinityMap[bindKey] = scRef
	}
	gb.affinityMap[bindKey].affinityCnt++
}

// unbindSubConn removes the existing binding associated with the key.
func (gb *gcpBalancer) unbindSubConn(boundKey string) {
	boundRef, ok := gb.affinityMap[boundKey]
	if ok {
		boundRef.affinityCnt--
		if boundRef.affinityCnt <= 0 {
			delete(gb.affinityMap, boundKey)
		}
	}
}

// regeneratePicker takes a snapshot of the balancer, and generates a picker
// from it. The picker is
//  - errPicker with ErrTransientFailure if the balancer is in TransientFailure,
//  - built by the pickerBuilder with all READY SubConns otherwise.
func (gb *gcpBalancer) regeneratePicker() {
	if gb.state == connectivity.TransientFailure {
		gb.picker = NewErrPicker(balancer.ErrTransientFailure)
		return
	}
	readyRefs := []*subConnRef{}

	// Select ready subConns from subConn map.
	for _, scRef := range gb.scRefs {
		if scRef.scState == connectivity.Ready {
			readyRefs = append(readyRefs, scRef)
		}
	}
	gb.picker = newGCPPicker(readyRefs, gb)
}

func (gb *gcpBalancer) HandleSubConnStateChange(sc balancer.SubConn, s connectivity.State) {
	grpclog.Infof("grpcgcp.gcpBalancer: handle SubConn state change: %p, %v", sc, s)
	scRef, ok := gb.scRefs[sc]
	if !ok {
		grpclog.Infof(
			"grpcgcp.gcpBalancer: got state changes for an unknown SubConn: %p, %v",
			sc,
			s,
		)
		return
	}
	oldS := scRef.scState
	scRef.scState = s
	switch s {
	case connectivity.Idle:
		sc.Connect()
	case connectivity.Shutdown:
		delete(gb.scRefs, sc)
	}

	oldAggrState := gb.state
	gb.state = gb.csEvltr.recordTransition(oldS, s)

	// Regenerate picker when one of the following happens:
	//  - this sc became ready from not-ready
	//  - this sc became not-ready from ready
	//  - the aggregated state of balancer became TransientFailure from non-TransientFailure
	//  - the aggregated state of balancer became non-TransientFailure from TransientFailure
	if (s == connectivity.Ready) != (oldS == connectivity.Ready) ||
		(gb.state == connectivity.TransientFailure) != (oldAggrState == connectivity.TransientFailure) {
		gb.regeneratePicker()
		gb.cc.UpdateBalancerState(gb.state, gb.picker)
	}
}

func (gb *gcpBalancer) Close() {
}
