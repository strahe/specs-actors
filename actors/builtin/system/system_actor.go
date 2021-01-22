package system

import (
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/cbor"
	rtt "github.com/filecoin-project/go-state-types/rt"
	"github.com/ipfs/go-cid"
	"time"

	"github.com/filecoin-project/specs-actors/v2/actors/builtin"
	"github.com/filecoin-project/specs-actors/v2/actors/runtime"
)

type Actor struct{}

func (a Actor) Exports() []interface{} {
	return []interface{}{
		builtin.MethodConstructor: a.Constructor,
	}
}

func (a Actor) Code() cid.Cid {
	return builtin.SystemActorCodeID
}

func (a Actor) IsSingleton() bool {
	return true
}

func (a Actor) State() cbor.Er {
	return new(State)
}

var _ runtime.VMActor = Actor{}

func (a Actor) Constructor(rt runtime.Runtime, _ *abi.EmptyValue) *abi.EmptyValue {
	start := time.Now()
	defer func() {
		if sp := time.Since(start); sp.Seconds() > 1 {
			rt.Log(rtt.WARN, "Constructor, took: %s", sp.String())
		}
	}()
	rt.ValidateImmediateCallerIs(builtin.SystemActorAddr)

	rt.StateCreate(&State{})
	return nil
}

type State struct{}
