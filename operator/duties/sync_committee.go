package duties

import (
	"context"
	"fmt"
	"strings"
	"time"

	eth2apiv1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	genesisspectypes "github.com/ssvlabs/ssv-spec-pre-cc/types"
	spectypes "github.com/ssvlabs/ssv-spec/types"
	"go.uber.org/zap"

	"github.com/ssvlabs/ssv/logging/fields"
	"github.com/ssvlabs/ssv/operator/duties/dutystore"
)

type SyncCommitteeHandler struct {
	baseHandler

	duties             *dutystore.SyncCommitteeDuties
	fetchCurrentPeriod bool
	fetchNextPeriod    bool

	// preparationSlots is the number of slots ahead of the sync committee
	// period change at which to prepare the relevant duties.
	preparationSlots uint64
}

func NewSyncCommitteeHandler(duties *dutystore.SyncCommitteeDuties) *SyncCommitteeHandler {
	h := &SyncCommitteeHandler{
		duties: duties,
	}
	h.fetchCurrentPeriod = true
	return h
}

func (h *SyncCommitteeHandler) Name() string {
	return spectypes.BNRoleSyncCommittee.String()
}

// HandleDuties manages the duty lifecycle, handling different cases:
//
// On First Run:
//  1. Fetch duties for the current period.
//  2. If necessary, fetch duties for the next period.
//  3. Execute duties.
//
// On Re-org:
//  1. Execute duties.
//  2. If necessary, fetch duties for the next period.
//
// On Indices Change:
//  1. Execute duties.
//  2. ResetEpoch duties for the current period.
//  3. Fetch duties for the current period.
//  4. If necessary, fetch duties for the next period.
//
// On Ticker event:
//  1. Execute duties.
//  2. If necessary, fetch duties for the next period.
func (h *SyncCommitteeHandler) HandleDuties(ctx context.Context) {
	h.logger.Info("starting duty handler")
	defer h.logger.Info("duty handler exited")

	// Prepare relevant duties 1.5 epochs (48 slots) ahead of the sync committee period change.
	// The 1.5 epochs timing helps ensure setup occurs when the beacon node is likely less busy.
	h.preparationSlots = h.network.Beacon.SlotsPerEpoch() * 3 / 2

	if h.shouldFetchNextPeriod(h.network.Beacon.EstimatedCurrentSlot()) {
		h.fetchNextPeriod = true
	}

	next := h.ticker.Next()
	for {
		select {
		case <-ctx.Done():
			return

		case <-next:
			slot := h.ticker.Slot()
			next = h.ticker.Next()
			epoch := h.network.Beacon.EstimatedEpochAtSlot(slot)
			period := h.network.Beacon.EstimatedSyncCommitteePeriodAtEpoch(epoch)
			buildStr := fmt.Sprintf("p%v-e%v-s%v-#%v", period, epoch, slot, slot%32+1)
			h.logger.Debug("🛠 ticker event", zap.String("period_epoch_slot_pos", buildStr))

			ctx, cancel := context.WithDeadline(ctx, h.network.Beacon.GetSlotStartTime(slot+1).Add(100*time.Millisecond))
			h.processExecution(period, slot)
			h.processFetching(ctx, period, true)
			cancel()

			// if we have reached the preparation slots -1, prepare the next period duties in the next slot.
			periodSlots := h.slotsPerPeriod()
			if uint64(slot)%periodSlots == periodSlots-h.preparationSlots-1 {
				h.fetchNextPeriod = true
			}

			// last slot of period
			if slot == h.network.Beacon.LastSlotOfSyncPeriod(period) {
				h.duties.Reset(period - 1)
			}

		case reorgEvent := <-h.reorg:
			epoch := h.network.Beacon.EstimatedEpochAtSlot(reorgEvent.Slot)
			period := h.network.Beacon.EstimatedSyncCommitteePeriodAtEpoch(epoch)

			buildStr := fmt.Sprintf("p%v-e%v-s%v-#%v", period, epoch, reorgEvent.Slot, reorgEvent.Slot%32+1)
			h.logger.Info("🔀 reorg event received", zap.String("period_epoch_slot_pos", buildStr), zap.Any("event", reorgEvent))

			// reset current epoch duties
			if reorgEvent.Current && h.shouldFetchNextPeriod(reorgEvent.Slot) {
				h.duties.Reset(period + 1)
				h.fetchNextPeriod = true
			}

		case <-h.indicesChange:
			slot := h.network.Beacon.EstimatedCurrentSlot()
			epoch := h.network.Beacon.EstimatedEpochAtSlot(slot)
			period := h.network.Beacon.EstimatedSyncCommitteePeriodAtEpoch(epoch)
			buildStr := fmt.Sprintf("p%v-e%v-s%v-#%v", period, epoch, slot, slot%32+1)
			h.logger.Info("🔁 indices change received", zap.String("period_epoch_slot_pos", buildStr))

			h.fetchCurrentPeriod = true

			// reset next period duties if in appropriate slot range
			if h.shouldFetchNextPeriod(slot) {
				h.fetchNextPeriod = true
			}
		}
	}
}

func (h *SyncCommitteeHandler) HandleInitialDuties(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, h.network.Beacon.SlotDurationSec()/2)
	defer cancel()

	epoch := h.network.Beacon.EstimatedCurrentEpoch()
	period := h.network.Beacon.EstimatedSyncCommitteePeriodAtEpoch(epoch)
	h.processFetching(ctx, period, false)
}

func (h *SyncCommitteeHandler) processFetching(ctx context.Context, period uint64, waitForInitial bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if h.fetchCurrentPeriod {
		if err := h.fetchAndProcessDuties(ctx, period, waitForInitial); err != nil {
			h.logger.Error("failed to fetch duties for current epoch", zap.Error(err))
			return
		}
		h.fetchCurrentPeriod = false
	}

	if h.fetchNextPeriod {
		if err := h.fetchAndProcessDuties(ctx, period+1, waitForInitial); err != nil {
			h.logger.Error("failed to fetch duties for next epoch", zap.Error(err))
			return
		}
		h.fetchNextPeriod = false
	}
}

func (h *SyncCommitteeHandler) processExecution(period uint64, slot phase0.Slot) {
	// range over duties and execute
	duties := h.duties.CommitteePeriodDuties(period)
	if duties == nil {
		return
	}

	if !h.network.PastAlanForkAtEpoch(h.network.Beacon.EstimatedEpochAtSlot(slot)) {
		toExecute := make([]*genesisspectypes.Duty, 0, len(duties)*2)
		for _, d := range duties {
			if h.shouldExecute(d, slot) {
				toExecute = append(toExecute, h.toGenesisSpecDuty(d, slot, genesisspectypes.BNRoleSyncCommittee))
				toExecute = append(toExecute, h.toGenesisSpecDuty(d, slot, genesisspectypes.BNRoleSyncCommitteeContribution))
			}
		}

		h.dutiesExecutor.ExecuteGenesisDuties(h.logger, toExecute)
		return
	}

	toExecute := make([]*spectypes.ValidatorDuty, 0, len(duties))
	for _, d := range duties {
		if h.shouldExecute(d, slot) {
			toExecute = append(toExecute, h.toSpecDuty(d, slot, spectypes.BNRoleSyncCommitteeContribution))
		}
	}

	h.dutiesExecutor.ExecuteDuties(h.logger, toExecute)
}

func (h *SyncCommitteeHandler) fetchAndProcessDuties(ctx context.Context, period uint64, waitForInitial bool) error {
	start := time.Now()
	firstEpoch := h.network.Beacon.FirstEpochOfSyncPeriod(period)
	currentEpoch := h.network.Beacon.EstimatedCurrentEpoch()
	if firstEpoch < currentEpoch {
		firstEpoch = currentEpoch
	}
	lastEpoch := h.network.Beacon.FirstEpochOfSyncPeriod(period+1) - 1

	allActiveIndices := h.validatorController.AllActiveIndices(firstEpoch, waitForInitial)
	if len(allActiveIndices) == 0 {
		h.logger.Debug("no active validators for period", fields.Epoch(currentEpoch), zap.Uint64("period", period))
		return nil
	}

	inCommitteeIndices := indicesFromShares(h.validatorProvider.SelfParticipatingValidators(firstEpoch))
	inCommitteeIndicesSet := map[phase0.ValidatorIndex]struct{}{}
	for _, idx := range inCommitteeIndices {
		inCommitteeIndicesSet[idx] = struct{}{}
	}

	duties, err := h.beaconNode.SyncCommitteeDuties(ctx, firstEpoch, allActiveIndices)
	if err != nil {
		return fmt.Errorf("failed to fetch sync committee duties: %w", err)
	}

	h.duties.Reset(period)
	for _, d := range duties {
		_, inCommitteeDuty := inCommitteeIndicesSet[d.ValidatorIndex]
		h.duties.Add(period, d.ValidatorIndex, d, inCommitteeDuty)
	}

	h.prepareDutiesResultLog(period, duties, start)

	// lastEpoch + 1 due to the fact that we need to subscribe "until" the end of the period
	subscriptions := calculateSubscriptions(lastEpoch+1, duties)
	if len(subscriptions) > 0 {
		if deadline, ok := ctx.Deadline(); ok {
			go func(h *SyncCommitteeHandler, subscriptions []*eth2apiv1.SyncCommitteeSubscription) {
				// Create a new subscription context with a deadline from parent context.
				subscriptionCtx, cancel := context.WithDeadline(context.Background(), deadline)
				defer cancel()
				if err := h.beaconNode.SubmitSyncCommitteeSubscriptions(subscriptionCtx, subscriptions); err != nil {
					h.logger.Warn("failed to subscribe sync committee to subnet", zap.Error(err))
				}
			}(h, subscriptions)
		} else {
			h.logger.Warn("failed to get context deadline")
		}
	}

	return nil
}

func (h *SyncCommitteeHandler) prepareDutiesResultLog(period uint64, duties []*eth2apiv1.SyncCommitteeDuty, start time.Time) {
	var b strings.Builder
	for i, duty := range duties {
		if i > 0 {
			b.WriteString(", ")
		}
		tmp := fmt.Sprintf("%v-p%v-v%v", h.Name(), period, duty.ValidatorIndex)
		b.WriteString(tmp)
	}
	h.logger.Debug("👥 got duties",
		fields.Count(len(duties)),
		zap.String("period", fmt.Sprintf("p%v", period)),
		zap.Any("duties", b.String()),
		fields.Duration(start))
}

func (h *SyncCommitteeHandler) toGenesisSpecDuty(duty *eth2apiv1.SyncCommitteeDuty, slot phase0.Slot, role genesisspectypes.BeaconRole) *genesisspectypes.Duty {
	indices := make([]uint64, len(duty.ValidatorSyncCommitteeIndices))
	for i, index := range duty.ValidatorSyncCommitteeIndices {
		indices[i] = uint64(index)
	}
	return &genesisspectypes.Duty{
		Type:                          role,
		PubKey:                        duty.PubKey,
		Slot:                          slot, // in order for the duty scheduler to execute
		ValidatorIndex:                duty.ValidatorIndex,
		ValidatorSyncCommitteeIndices: indices,
	}
}

func (h *SyncCommitteeHandler) toSpecDuty(duty *eth2apiv1.SyncCommitteeDuty, slot phase0.Slot, role spectypes.BeaconRole) *spectypes.ValidatorDuty {
	indices := make([]uint64, len(duty.ValidatorSyncCommitteeIndices))
	for i, index := range duty.ValidatorSyncCommitteeIndices {
		indices[i] = uint64(index)
	}
	return &spectypes.ValidatorDuty{
		Type:                          role,
		PubKey:                        duty.PubKey,
		Slot:                          slot, // in order for the duty scheduler to execute
		ValidatorIndex:                duty.ValidatorIndex,
		ValidatorSyncCommitteeIndices: indices,
	}
}

func (h *SyncCommitteeHandler) shouldExecute(duty *eth2apiv1.SyncCommitteeDuty, slot phase0.Slot) bool {
	currentSlot := h.network.Beacon.EstimatedCurrentSlot()
	// execute task if slot already began and not pass 1 slot
	if currentSlot == slot {
		return true
	}
	if currentSlot+1 == slot {
		h.warnMisalignedSlotAndDuty(duty.String())
		return true
	}
	return false
}

// calculateSubscriptions calculates the sync committee subscriptions given a set of duties.
func calculateSubscriptions(endEpoch phase0.Epoch, duties []*eth2apiv1.SyncCommitteeDuty) []*eth2apiv1.SyncCommitteeSubscription {
	subscriptions := make([]*eth2apiv1.SyncCommitteeSubscription, 0, len(duties))
	for _, duty := range duties {
		subscriptions = append(subscriptions, &eth2apiv1.SyncCommitteeSubscription{
			ValidatorIndex:       duty.ValidatorIndex,
			SyncCommitteeIndices: duty.ValidatorSyncCommitteeIndices,
			UntilEpoch:           endEpoch,
		})
	}

	return subscriptions
}

func (h *SyncCommitteeHandler) shouldFetchNextPeriod(slot phase0.Slot) bool {
	periodSlots := h.slotsPerPeriod()
	return uint64(slot)%periodSlots > periodSlots-h.preparationSlots-2
}

func (h *SyncCommitteeHandler) slotsPerPeriod() uint64 {
	return h.network.Beacon.EpochsPerSyncCommitteePeriod() * h.network.Beacon.SlotsPerEpoch()
}
