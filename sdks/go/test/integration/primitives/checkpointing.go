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

package primitives

import (
	"reflect"
	"time"

	"github.com/apache/beam/sdks/v2/go/pkg/beam"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/core/sdf"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/io/rtrackers/offsetrange"
	"github.com/apache/beam/sdks/v2/go/pkg/beam/testing/passert"
)

func init() {
	beam.RegisterType(reflect.TypeOf((*selfCheckpointingDoFn)(nil)).Elem())
}

type selfCheckpointingDoFn struct{}

// CreateInitialRestriction creates the restriction being used by the SDF. In this case, the range
// of values produced by the restriction is [Start, End).
func (fn *selfCheckpointingDoFn) CreateInitialRestriction(_ []byte) offsetrange.Restriction {
	return offsetrange.Restriction{
		Start: int64(0),
		End:   int64(10),
	}
}

// CreateTracker wraps the fiven restriction into a LockRTracker type.
func (fn *selfCheckpointingDoFn) CreateTracker(rest offsetrange.Restriction) *sdf.LockRTracker {
	return sdf.NewLockRTracker(offsetrange.NewTracker(rest))
}

// RestrictionSize returns the size of the current restriction
func (fn *selfCheckpointingDoFn) RestrictionSize(_ []byte, rest offsetrange.Restriction) float64 {
	return rest.Size()
}

// SplitRestriction modifies the offsetrange.Restriction's sized restriction function to produce a size-zero restriction
// at the end of execution.
func (fn *selfCheckpointingDoFn) SplitRestriction(_ []byte, rest offsetrange.Restriction) []offsetrange.Restriction {
	size := int64(1)
	s := rest.Start
	var splits []offsetrange.Restriction
	for e := s + size; e <= rest.End; s, e = e, e+size {
		splits = append(splits, offsetrange.Restriction{Start: s, End: e})
	}
	splits = append(splits, offsetrange.Restriction{Start: s, End: rest.End})
	return splits
}

// ProcessElement continually gets the start position of the restriction and emits it as an int64 value before checkpointing.
// This causes the restriction to be split after the claimed work and produce no primary roots.
func (fn *selfCheckpointingDoFn) ProcessElement(rt *sdf.LockRTracker, _ []byte, emit func(int64)) sdf.ProcessContinuation {
	position := rt.GetRestriction().(offsetrange.Restriction).Start

	for {
		if rt.TryClaim(position) {
			// Successful claim, emit the value and move on.
			emit(position)
			position++
			return sdf.ResumeProcessingIn(1 * time.Second)
		} else if rt.GetError() != nil || rt.IsDone() {
			// Stop processing on error or completion
			return sdf.StopProcessing()
		} else {
			// Failed to claim but no error, resume later.
			return sdf.ResumeProcessingIn(5 * time.Second)
		}
	}
}

// Checkpoints is a small test pipeline to establish the correctness of the simple test case.
func Checkpoints(s beam.Scope) {
	beam.Init()

	s.Scope("checkpoint")
	out := beam.ParDo(s, &selfCheckpointingDoFn{}, beam.Impulse(s))
	passert.Count(s, out, "num ints", 10)
}
