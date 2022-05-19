/*
Copyright 2022 The VolSync authors.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package statemachine

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	volsyncv1alpha1 "github.com/backube/volsync/api/v1alpha1"
	"github.com/backube/volsync/controllers/mover"
)

var ctx = context.Background()
var logger = zap.New(zap.UseDevMode(true), zap.WriteTo(GinkgoWriter))

var _ = Describe("State transitions", func() {
	It("an uninitialized machine will move to Syncing", func() {
		m := newFakeMachine()
		Expect(currentState(m)).To(Equal(initialState))
		_, err := Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(synchronizingState))
		// Brand new, so we're out of sync
		Expect(m.OOSync).To(BeTrue())
	})
	It("will keep syncing until it completes", func() {
		m := newFakeMachine()
		// Force syncing state
		Expect(transitionToSynchronizing(m, logger)).To(Succeed())
		Expect(currentState(m)).To(Equal(synchronizingState))
		Expect(apimeta.IsStatusConditionTrue(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())

		// Not complete so we stay in syncing
		m.SyncResult = mover.InProgress()
		_, err := Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(synchronizingState))
		Expect(apimeta.IsStatusConditionTrue(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())

		m.SyncErr = fmt.Errorf("error")
		_, err = Run(ctx, m, logger)
		Expect(err).To(HaveOccurred())
		Expect(currentState(m)).To(Equal(synchronizingState))
		Expect(apimeta.IsStatusConditionFalse(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())
		Expect(apimeta.FindStatusCondition(m.Cond,
			volsyncv1alpha1.ConditionSynchronizing).Reason).To(Equal(volsyncv1alpha1.SynchronizingReasonError))

		// Complete takes us to cleanup
		m.SyncResult, m.SyncErr = mover.Complete(), nil
		_, err = Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(cleaningUpState))
		Expect(apimeta.IsStatusConditionFalse(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())
		// Just finished a sync, so we are in-sync
		Expect(m.OOSync).To(BeFalse())
	})
	It("will cleanup until complete", func() {
		m := newFakeMachine()
		// Force cleanup state
		Expect(transitionToSynchronizing(m, logger)).To(Succeed())
		Expect(transitionToCleaningUp(m, logger)).To(Succeed())
		Expect(currentState(m)).To(Equal(cleaningUpState))

		m.CleanupResult = mover.InProgress()
		_, err := Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(cleaningUpState))
		Expect(apimeta.IsStatusConditionFalse(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())

		m.CleanupError = fmt.Errorf("err")
		_, err = Run(ctx, m, logger)
		Expect(err).To(HaveOccurred())
		Expect(currentState(m)).To(Equal(cleaningUpState))
		Expect(apimeta.IsStatusConditionTrue(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeFalse())
		Expect(apimeta.FindStatusCondition(m.Cond,
			volsyncv1alpha1.ConditionSynchronizing).Reason).To(Equal(volsyncv1alpha1.SynchronizingReasonError))

		m.CleanupResult = mover.Complete()
		m.CleanupError = nil
		_, err = Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).ToNot(Equal(cleaningUpState))
	})
})

var _ = When("in cleanup", func() {
	var m *fakeMachine
	BeforeEach(func() {
		m = newFakeMachine()
		m.SyncResult = mover.Complete()
	})
	JustBeforeEach(func() {
		_, err := Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		_, err = Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(cleaningUpState))
	})
	It("starts syncing if no trigger", func() {
		m.CleanupResult = mover.Complete()
		m.TT = noTrigger
		_, err := Run(ctx, m, logger)
		Expect(err).ToNot(HaveOccurred())
		Expect(currentState(m)).To(Equal(synchronizingState))
	})
	When("the trigger is manual", func() {
		BeforeEach(func() {
			m.TT = manualTrigger
			m.MT = "1"
			m.LMT = "1"
		})
		It("waits for trigger if manual", func() {
			m.CleanupResult = mover.Complete()
			// Run a few times
			_, _ = Run(ctx, m, logger)
			_, _ = Run(ctx, m, logger)
			_, err := Run(ctx, m, logger)
			Expect(err).ToNot(HaveOccurred())
			Expect(currentState(m)).To(Equal(cleaningUpState))
			Expect(apimeta.IsStatusConditionFalse(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())
			Expect(apimeta.FindStatusCondition(m.Cond,
				volsyncv1alpha1.ConditionSynchronizing).Reason).To(Equal(volsyncv1alpha1.SynchronizingReasonManual))

			// Should transition when we trigger it
			m.MT = "2"
			_, err = Run(ctx, m, logger)
			Expect(err).ToNot(HaveOccurred())
			Expect(currentState(m)).To(Equal(synchronizingState))
			Expect(apimeta.IsStatusConditionTrue(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())
		})
	})
	When("the trigger is scheduled", func() {
		BeforeEach(func() {
			m.TT = scheduleTrigger
			m.CS = "0 0 1 1 *"
		})
		It("waits for schedule if scheduled", func() {
			m.CleanupResult = mover.Complete()
			// Run a few times
			_, _ = Run(ctx, m, logger)
			_, _ = Run(ctx, m, logger)
			_, err := Run(ctx, m, logger)
			Expect(err).ToNot(HaveOccurred())
			Expect(currentState(m)).To(Equal(cleaningUpState))
			Expect(apimeta.IsStatusConditionFalse(m.Cond, volsyncv1alpha1.ConditionSynchronizing)).To(BeTrue())
			Expect(apimeta.FindStatusCondition(m.Cond,
				volsyncv1alpha1.ConditionSynchronizing).Reason).To(Equal(volsyncv1alpha1.SynchronizingReasonSched))
		})
	})
})

var _ = Describe("missedDeadline", func() {
	var m *fakeMachine
	BeforeEach(func() {
		m = newFakeMachine()
		m.TT = scheduleTrigger
		m.CS = "* * * * *"
	})
	It("Deadline is not missed if we've never synced", func() {
		miss, err := missedDeadline(m)
		Expect(miss).To(BeFalse())
		Expect(err).ToNot(HaveOccurred())
	})
	It("Deadline is not missed if we synced w/in 2 periods", func() {
		// every 10 min
		m.CS = "*/10 * * * *"
		last := time.Now().Add(-9 * time.Minute)
		m.LST = &metav1.Time{Time: last}
		miss, err := missedDeadline(m)
		Expect(miss).To(BeFalse())
		Expect(err).ToNot(HaveOccurred())
	})
	It("Deadline IS missed if we synced longer than 2 periods ago", func() {
		m.CS = "*/10 * * * *"
		// To Sync
		_, _ = Run(ctx, m, logger)
		// To Cleanup
		_, _ = Run(ctx, m, logger)

		// Set last sync time back to make it look like we're really late
		last := time.Now().Add(-31 * time.Minute)
		m.LST = &metav1.Time{Time: last}
		next := last.Add(2 * time.Minute)
		m.NST = &metav1.Time{Time: next}

		miss, err := missedDeadline(m)
		Expect(miss).To(BeTrue())
		Expect(err).ToNot(HaveOccurred())

		// If we Run, it'll set the OOS metric & start syncing
		Expect(m.OOSync).To(BeFalse())
		_, _ = Run(ctx, m, logger)
		Expect(m.OOSync).To(BeTrue())
		Expect(currentState(m)).To(Equal(synchronizingState))
	})
})

var _ = When("the trigger is schedule-based", func() {
	It("returns an error if the cronspec is invalid", func() {
		m := newFakeMachine()
		m.TT = scheduleTrigger
		m.CS = "invalid"
		Expect(currentState(m)).To(Equal(initialState))
		_, _ = Run(ctx, m, logger)
		Expect(currentState(m)).To(Equal(synchronizingState))
		_, err := Run(ctx, m, logger)
		Expect(err).To(HaveOccurred())
		c := apimeta.FindStatusCondition(m.Cond, volsyncv1alpha1.ConditionSynchronizing)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal(volsyncv1alpha1.SynchronizingReasonError))
	})
})