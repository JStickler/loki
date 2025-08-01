// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package internal // import "go.opentelemetry.io/collector/pdata/internal"

import (
	otlpcommon "go.opentelemetry.io/collector/pdata/internal/data/protogen/common/v1"
	"go.opentelemetry.io/collector/pdata/internal/json"
)

type Slice struct {
	orig  *[]otlpcommon.AnyValue
	state *State
}

func GetOrigSlice(ms Slice) *[]otlpcommon.AnyValue {
	return ms.orig
}

func GetSliceState(ms Slice) *State {
	return ms.state
}

func NewSlice(orig *[]otlpcommon.AnyValue, state *State) Slice {
	return Slice{orig: orig, state: state}
}

func CopyOrigSlice(dest, src []otlpcommon.AnyValue) []otlpcommon.AnyValue {
	if cap(dest) < len(src) {
		dest = make([]otlpcommon.AnyValue, len(src))
	}
	dest = dest[:len(src)]
	for i := 0; i < len(src); i++ {
		CopyOrigValue(&dest[i], &src[i])
	}
	return dest
}

func GenerateTestSlice() Slice {
	orig := []otlpcommon.AnyValue{}
	state := StateMutable
	tv := NewSlice(&orig, &state)
	FillTestSlice(tv)
	return tv
}

func FillTestSlice(tv Slice) {
	*tv.orig = make([]otlpcommon.AnyValue, 7)
	for i := 0; i < 7; i++ {
		state := StateMutable
		FillTestValue(NewValue(&(*tv.orig)[i], &state))
	}
}

// MarshalJSONStreamSlice marshals all properties from the current struct to the destination stream.
func MarshalJSONStreamSlice(ms Slice, dest *json.Stream) {
	dest.WriteArrayStart()
	if len(*ms.orig) > 0 {
		MarshalJSONStreamValue(NewValue(&(*ms.orig)[0], ms.state), dest)
	}
	for i := 1; i < len(*ms.orig); i++ {
		dest.WriteMore()
		MarshalJSONStreamValue(NewValue(&(*ms.orig)[i], ms.state), dest)
	}
	dest.WriteArrayEnd()
}

func UnmarshalJSONIterSlice(ms Slice, iter *json.Iterator) {
	iter.ReadArrayCB(func(iter *json.Iterator) bool {
		*ms.orig = append(*ms.orig, otlpcommon.AnyValue{})
		UnmarshalJSONIterValue(NewValue(&(*ms.orig)[len(*ms.orig)-1], ms.state), iter)
		return true
	})
}
