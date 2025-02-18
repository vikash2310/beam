// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package offsetrange defines a restriction and restriction tracker for offset
// ranges. An offset range is just a range, with a start and end, that can
// begin at an offset, and is commonly used to represent byte ranges for files
// or indices for iterable containers.
package offsetrange

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"reflect"

	"github.com/apache/beam/sdks/v2/go/pkg/beam/core/graph/coder"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/core/runtime"
)

func init() {
	runtime.RegisterType(reflect.TypeOf((*Tracker)(nil)))
	runtime.RegisterType(reflect.TypeOf((*Restriction)(nil)).Elem())
	runtime.RegisterFunction(restEnc)
	runtime.RegisterFunction(restDec)
	coder.RegisterCoder(reflect.TypeOf((*Restriction)(nil)).Elem(), restEnc, restDec)
}

func restEnc(in Restriction) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, in); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func restDec(in []byte) (Restriction, error) {
	buf := bytes.NewBuffer(in)
	rest := Restriction{}
	if err := binary.Read(buf, binary.BigEndian, &rest); err != nil {
		return rest, err
	}
	return rest, nil
}

// Restriction is an offset range restriction, which represents a range of
// integers as a half-closed interval with boundaries [start, end).
type Restriction struct {
	Start, End int64
}

// EvenSplits splits a restriction into a number of evenly sized restrictions
// in ascending order. Each split restriction is guaranteed to not be empty, and
// each unit from the original restriction is guaranteed to be contained in one
// split restriction.
//
// Num should be greater than 0. Otherwise there is no way to split the
// restriction and this function will return the original restriction.
func (r Restriction) EvenSplits(num int64) (splits []Restriction) {
	if num <= 1 {
		// Don't split, just return original restriction.
		return append(splits, r)
	}

	offset := r.Start
	size := r.End - r.Start
	for i := int64(0); i < num; i++ {
		split := Restriction{
			Start: offset + (i * size / num),
			End:   offset + ((i + 1) * size / num),
		}
		// Skip restrictions that end up empty.
		if split.End-split.Start <= 0 {
			continue
		}
		splits = append(splits, split)
	}
	return splits
}

// SizedSplits splits a restriction into multiple restrictions of the given
// size, in ascending order. If the restriction cannot be evenly split, the
// final restriction will be the remainder.
//
// Example: (0, 24) split into size 10s -> {(0, 10), (10, 20), (20, 24)}
//
// Size should be greater than 0. Otherwise there is no way to split the
// restriction and this function will return the original restriction.
func (r Restriction) SizedSplits(size int64) (splits []Restriction) {
	if size < 1 {
		// Don't split, just return original restriction.
		return append(splits, r)
	}

	s := r.Start
	for e := s + size; e < r.End; s, e = e, e+size {
		splits = append(splits, Restriction{Start: s, End: e})
	}
	splits = append(splits, Restriction{Start: s, End: r.End})
	return splits
}

// Size returns the restriction's size as the difference between Start and End.
func (r Restriction) Size() float64 {
	return float64(r.End - r.Start)
}

// Tracker tracks a restriction  that can be represented as a range of integer values,
// for example for byte offsets in a file, or indices in an array. Note that this tracker makes
// no assumptions about the positions of blocks within the range, so users must handle validation
// of block positions if needed.
type Tracker struct {
	rest    Restriction
	claimed int64 // Tracks the last claimed position.
	stopped bool  // Tracks whether TryClaim has indicated to stop processing elements.
	err     error
}

// NewTracker is a constructor for an Tracker given a start and end range.
func NewTracker(rest Restriction) *Tracker {
	return &Tracker{
		rest:    rest,
		claimed: rest.Start - 1,
		stopped: false,
		err:     nil,
	}
}

// TryClaim accepts an int64 position representing the starting position of a block of work. It
// successfully claims it if the position is greater than the previously claimed position and within
// the restriction. Claiming a position at or beyond the end of the restriction signals that the
// entire restriction has been processed and is now done, at which point this method signals to end
// processing.
//
// The tracker stops with an error if a claim is attempted after the tracker has signalled to stop,
// if a position is claimed before the start of the restriction, or if a position is claimed before
// the latest successfully claimed.
func (tracker *Tracker) TryClaim(rawPos interface{}) bool {
	if tracker.stopped {
		tracker.err = errors.New("cannot claim work after restriction tracker returns false")
		return false
	}

	pos := rawPos.(int64)

	if pos < tracker.rest.Start {
		tracker.stopped = true
		tracker.err = errors.New("position claimed is out of bounds of the restriction")
		return false
	}
	if pos <= tracker.claimed {
		tracker.stopped = true
		tracker.err = errors.New("cannot claim a position lower than the previously claimed position")
		return false
	}

	tracker.claimed = pos
	if pos >= tracker.rest.End {
		tracker.stopped = true
		return false
	}
	return true
}

// GetError returns the error that caused the tracker to stop, if there is one.
func (tracker *Tracker) GetError() error {
	return tracker.err
}

// TrySplit splits at the nearest integer greater than the given fraction of the remainder. If the
// fraction given is outside of the [0, 1] range, it is clamped to 0 or 1.
func (tracker *Tracker) TrySplit(fraction float64) (primary, residual interface{}, err error) {
	if tracker.stopped || tracker.IsDone() {
		return tracker.rest, nil, nil
	}
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1 {
		fraction = 1
	}

	// Use Ceil to always round up from float split point.
	splitPt := tracker.claimed + int64(math.Ceil(fraction*float64(tracker.rest.End-tracker.claimed)))
	if splitPt >= tracker.rest.End {
		return tracker.rest, nil, nil
	}
	residual = Restriction{splitPt, tracker.rest.End}
	tracker.rest.End = splitPt
	return tracker.rest, residual, nil
}

// GetProgress reports progress based on the claimed size and unclaimed sizes of the restriction.
func (tracker *Tracker) GetProgress() (done, remaining float64) {
	done = float64((tracker.claimed + 1) - tracker.rest.Start)
	remaining = float64(tracker.rest.End - (tracker.claimed + 1))
	return
}

// IsDone returns true if the most recent claimed element is past the end of the restriction.
func (tracker *Tracker) IsDone() bool {
	return tracker.err == nil && tracker.claimed >= tracker.rest.End
}

// GetRestriction returns a copy of the tracker's underlying offsetrange.Restriction.
func (tracker *Tracker) GetRestriction() interface{} {
	return tracker.rest
}

// IsBounded returns whether or not the restriction tracker is tracking a bounded restriction
// that has a set maximum value or an unbounded one which can grow indefinitely.
func (tracker *Tracker) IsBounded() bool {
	return true
}
