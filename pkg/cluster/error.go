// Copyright 2016 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"github.com/pkg/errors"
)

var (
	errCreatingCluster = errors.New("cluster failed to be created")
	errQuorumLost      = newFatalError("quorum was lost")
)

type fatalError struct {
	reason string
}

func (fe *fatalError) Error() string {
	return fe.reason
}

func newFatalError(reason string) *fatalError {
	return &fatalError{reason}
}

func isFatalError(err error) bool {
	switch errors.Cause(err).(type) {
	case *fatalError:
		return true
	default:
		return false
	}
}
