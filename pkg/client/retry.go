// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package client

// RetryOnConflict is the client-side sibling of the store's
// GuaranteedUpdate, forked from the pattern in k8s's
// client-go/util/retry (Copyright The Kubernetes Authors, Apache-2.0).

import (
	"errors"
	"time"

	"github.com/mroberts91/imp/api/v1alpha1"
)

const retryAttempts = 5

func RetryOnConflict(fn func() error) error {
	var err error
	for attempt := range retryAttempts {
		if err = fn(); !errors.Is(err, v1alpha1.ErrConflict) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}
