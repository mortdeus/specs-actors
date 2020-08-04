package miner

import (
	"bytes"
	"errors"

	"github.com/filecoin-project/go-bitfield"
	"github.com/ipfs/go-cid"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	xc "github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
)

// Deadlines contains Deadline objects, describing the sectors due at the given
// deadline and their state (faulty, terminated, recovering, etc.).
type Deadlines struct {
	// Note: we could inline part of the deadline struct (e.g., active/assigned sectors)
	// to make new sector assignment cheaper. At the moment, assigning a sector requires
	// loading all deadlines to figure out where best to assign new sectors.
	Due [WPoStPeriodDeadlines]cid.Cid // []Deadline
}

// Deadline holds the state for all sectors due at a specific deadline.
type Deadline struct {
	// Partitions in this deadline, in order.
	// The keys of this AMT are always sequential integers beginning with zero.
	Partitions cid.Cid // AMT[PartitionNumber]Partition

	// Maps epochs to partitions that _may_ have sectors that expire in or
	// before that epoch, either on-time or early as faults.
	// Keys are quantized to final epochs in each proving deadline.
	//
	// NOTE: Partitions MUST NOT be removed from this queue (until the
	// associated epoch has passed) even if they no longer have sectors
	// expiring at that epoch. Sectors expiring at this epoch may later be
	// recovered, and this queue will not be updated at that time.
	ExpirationsEpochs cid.Cid // AMT[ChainEpoch]BitField

	// Partitions numbers with PoSt submissions since the proving period started.
	PostSubmissions *abi.BitField

	// Partitions with sectors that terminated early.
	EarlyTerminations *abi.BitField

	// The number of non-terminated sectors in this deadline (incl faulty).
	LiveSectors uint64

	// The total number of sectors in this deadline (incl dead).
	TotalSectors uint64

	// Memoized sum of faulty power in partitions.
	FaultyPower PowerPair
}

//
// Deadlines (plural)
//

func ConstructDeadlines(emptyDeadlineCid cid.Cid) *Deadlines {
	d := new(Deadlines)
	for i := range d.Due {
		d.Due[i] = emptyDeadlineCid
	}
	return d
}

func (d *Deadlines) LoadDeadline(store adt.Store, dlIdx uint64) (*Deadline, error) {
	if dlIdx >= uint64(len(d.Due)) {
		return nil, xc.ErrIllegalArgument.Wrapf("invalid deadline %d", dlIdx)
	}
	deadline := new(Deadline)
	err := store.Get(store.Context(), d.Due[dlIdx], deadline)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to lookup deadline %d: %w", dlIdx, err)
	}
	return deadline, nil
}

func (d *Deadlines) ForEach(store adt.Store, cb func(dlIdx uint64, dl *Deadline) error) error {
	for dlIdx := range d.Due {
		dl, err := d.LoadDeadline(store, uint64(dlIdx))
		if err != nil {
			return err
		}
		err = cb(uint64(dlIdx), dl)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Deadlines) UpdateDeadline(store adt.Store, dlIdx uint64, deadline *Deadline) error {
	if dlIdx >= uint64(len(d.Due)) {
		return xerrors.Errorf("invalid deadline %d", dlIdx)
	}
	dlCid, err := store.Put(store.Context(), deadline)
	if err != nil {
		return err
	}
	d.Due[dlIdx] = dlCid
	return nil
}

//
// Deadline (singular)
//

func ConstructDeadline(emptyArrayCid cid.Cid) *Deadline {
	return &Deadline{
		Partitions:        emptyArrayCid,
		ExpirationsEpochs: emptyArrayCid,
		PostSubmissions:   abi.NewBitField(),
		EarlyTerminations: abi.NewBitField(),
		LiveSectors:       0,
		TotalSectors:      0,
		FaultyPower:       NewPowerPairZero(),
	}
}

func (d *Deadline) PartitionsArray(store adt.Store) (*adt.Array, error) {
	arr, err := adt.AsArray(store, d.Partitions)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to load partitions: %w", err)
	}
	return arr, nil
}

func (d *Deadline) LoadPartition(store adt.Store, partIdx uint64) (*Partition, error) {
	partitions, err := d.PartitionsArray(store)
	if err != nil {
		return nil, err
	}
	var partition Partition
	found, err := partitions.Get(partIdx, &partition)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to lookup partition %d: %w", partIdx, err)
	}
	if !found {
		return nil, xc.ErrNotFound.Wrapf("no partition %d", partIdx)
	}
	return &partition, nil
}

// Adds some partition numbers to the set expiring at an epoch.
func (d *Deadline) AddExpirationPartitions(store adt.Store, expirationEpoch abi.ChainEpoch, partitions []uint64, quant QuantSpec) error {
	queue, err := LoadBitfieldQueue(store, d.ExpirationsEpochs, quant)
	if err != nil {
		return xerrors.Errorf("failed to load expiration queue: %w", err)
	}
	if err = queue.AddToQueueValues(expirationEpoch, partitions...); err != nil {
		return xerrors.Errorf("failed to mutate expiration queue: %w", err)
	}
	if d.ExpirationsEpochs, err = queue.Root(); err != nil {
		return xerrors.Errorf("failed to save expiration queue: %w", err)
	}
	return nil
}

// PopExpiredSectors terminates expired sectors from all partitions.
// Returns the expired sector aggregates.
func (dl *Deadline) PopExpiredSectors(store adt.Store, until abi.ChainEpoch, quant QuantSpec) (*ExpirationSet, error) {
	expiredPartitions, modified, err := dl.popExpiredPartitions(store, until, quant)
	if err != nil {
		return nil, err
	} else if !modified {
		return NewExpirationSetEmpty(), nil // nothing to do.
	}

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, err
	}

	var onTimeSectors []*abi.BitField
	var earlySectors []*abi.BitField
	allOnTimePledge := big.Zero()
	allActivePower := NewPowerPairZero()
	allFaultyPower := NewPowerPairZero()
	var partitionsWithEarlyTerminations []uint64

	// For each partition with an expiry, remove and collect expirations from the partition queue.
	if err = expiredPartitions.ForEach(func(partIdx uint64) error {
		var partition Partition
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return err
		} else if !found {
			return xerrors.Errorf("missing expected partition %d", partIdx)
		}

		partExpiration, err := partition.PopExpiredSectors(store, until, quant)
		if err != nil {
			return xerrors.Errorf("failed to pop expired sectors from partition %d: %w", partIdx, err)
		}

		onTimeSectors = append(onTimeSectors, partExpiration.OnTimeSectors)
		earlySectors = append(earlySectors, partExpiration.EarlySectors)
		allActivePower = allActivePower.Add(partExpiration.ActivePower)
		allFaultyPower = allFaultyPower.Add(partExpiration.FaultyPower)
		allOnTimePledge = big.Add(allOnTimePledge, partExpiration.OnTimePledge)

		if empty, err := partExpiration.EarlySectors.IsEmpty(); err != nil {
			return xerrors.Errorf("failed to count early expirations from partition %d: %w", partIdx, err)
		} else if !empty {
			partitionsWithEarlyTerminations = append(partitionsWithEarlyTerminations, partIdx)
		}

		return partitions.Set(partIdx, &partition)
	}); err != nil {
		return nil, err
	}

	if dl.Partitions, err = partitions.Root(); err != nil {
		return nil, err
	}

	// Update early expiration bitmap.
	for _, partIdx := range partitionsWithEarlyTerminations {
		dl.EarlyTerminations.Set(partIdx)
	}

	allOnTimeSectors, err := bitfield.MultiMerge(onTimeSectors...)
	if err != nil {
		return nil, err
	}
	allEarlySectors, err := bitfield.MultiMerge(earlySectors...)
	if err != nil {
		return nil, err
	}

	// Update live sector count.
	onTimeCount, err := allOnTimeSectors.Count()
	if err != nil {
		return nil, xerrors.Errorf("failed to count on-time expired sectors: %w", err)
	}
	earlyCount, err := allEarlySectors.Count()
	if err != nil {
		return nil, xerrors.Errorf("failed to count early expired sectors: %w", err)
	}
	dl.LiveSectors -= onTimeCount + earlyCount

	dl.FaultyPower = dl.FaultyPower.Sub(allFaultyPower)

	return NewExpirationSet(allOnTimeSectors, allEarlySectors, allOnTimePledge, allActivePower, allFaultyPower), nil
}

// Adds sectors to a deadline. It's the caller's responsibility to make sure
// that this deadline isn't currently "open" (i.e., being proved at this point
// in time).
// The sectors are assumed to be non-faulty.
func (dl *Deadline) AddSectors(store adt.Store, partitionSize uint64, sectors []*SectorOnChainInfo,
	ssize abi.SectorSize, quant QuantSpec) (PowerPair, error) {
	if len(sectors) == 0 {
		return NewPowerPairZero(), nil
	}

	// First update partitions, consuming the sectors
	partitionDeadlineUpdates := make(map[abi.ChainEpoch][]uint64)
	newPower := NewPowerPairZero()
	dl.LiveSectors += uint64(len(sectors))
	dl.TotalSectors += uint64(len(sectors))

	{
		partitions, err := dl.PartitionsArray(store)
		if err != nil {
			return NewPowerPairZero(), err
		}

		partIdx := partitions.Length()
		if partIdx > 0 {
			partIdx -= 1 // try filling up the last partition first.
		}

		for ; len(sectors) > 0; partIdx++ {
			// Get/create partition to update.
			partition := new(Partition)
			if found, err := partitions.Get(partIdx, partition); err != nil {
				return NewPowerPairZero(), err
			} else if !found {
				// This case will usually happen zero times.
				// It would require adding more than a full partition in one go
				// to happen more than once.
				emptyArray, err := adt.MakeEmptyArray(store).Root()
				if err != nil {
					return NewPowerPairZero(), err
				}
				partition = ConstructPartition(emptyArray)
			}

			// Figure out which (if any) sectors we want to add to this partition.
			sectorCount, err := partition.Sectors.Count()
			if err != nil {
				return NewPowerPairZero(), err
			}
			if sectorCount >= partitionSize {
				continue
			}

			size := min64(partitionSize-sectorCount, uint64(len(sectors)))
			partitionNewSectors := sectors[:size]
			sectors = sectors[size:]

			// Add sectors to partition.
			partitionNewPower, err := partition.AddSectors(store, partitionNewSectors, ssize, quant)
			if err != nil {
				return NewPowerPairZero(), err
			}
			newPower = newPower.Add(partitionNewPower)

			// Save partition back.
			err = partitions.Set(partIdx, partition)
			if err != nil {
				return NewPowerPairZero(), err
			}

			// Record deadline -> partition mapping so we can later update the deadlines.
			for _, sector := range partitionNewSectors {
				partitionUpdate := partitionDeadlineUpdates[sector.Expiration]
				// Record each new partition once.
				if len(partitionUpdate) > 0 && partitionUpdate[len(partitionUpdate)-1] == partIdx {
					continue
				}
				partitionDeadlineUpdates[sector.Expiration] = append(partitionUpdate, partIdx)
			}
		}

		// Save partitions back.
		dl.Partitions, err = partitions.Root()
		if err != nil {
			return NewPowerPairZero(), err
		}
	}

	// Next, update the expiration queue.
	{
		deadlineExpirations, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant)
		if err != nil {
			return NewPowerPairZero(), xerrors.Errorf("failed to load expiration epochs: %w", err)
		}

		if err = deadlineExpirations.AddManyToQueueValues(partitionDeadlineUpdates); err != nil {
			return NewPowerPairZero(), xerrors.Errorf("failed to add expirations for new deadlines: %w", err)
		}

		if dl.ExpirationsEpochs, err = deadlineExpirations.Root(); err != nil {
			return NewPowerPairZero(), err
		}
	}

	return newPower, nil
}

func (dl *Deadline) PopEarlyTerminations(store adt.Store, maxPartitions, maxSectors uint64) (result TerminationResult, hasMore bool, err error) {
	stopErr := errors.New("stop error")

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return TerminationResult{}, false, err
	}

	var partitionsFinished []uint64
	if err = dl.EarlyTerminations.ForEach(func(partIdx uint64) error {
		// Load partition.
		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		}

		if !found {
			// If the partition doesn't exist any more, no problem.
			// We don't expect this to happen (compaction should re-index altered partitions),
			// but it's not worth failing if it does.
			partitionsFinished = append(partitionsFinished, partIdx)
			return nil
		}

		// Pop early terminations.
		partitionResult, more, err := partition.PopEarlyTerminations(
			store, maxSectors-result.SectorsProcessed,
		)
		if err != nil {
			return xerrors.Errorf("failed to pop terminations from partition: %w", err)
		}

		err = result.Add(partitionResult)
		if err != nil {
			return xerrors.Errorf("failed to merge termination result: %w", err)
		}

		// If we've processed all of them for this partition, unmark it in the deadline.
		if !more {
			partitionsFinished = append(partitionsFinished, partIdx)
		}

		// Save partition
		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to store partition %v", partIdx)
		}

		if result.BelowLimit(maxPartitions, maxSectors) {
			return nil
		}

		return stopErr
	}); err != nil && err != stopErr {
		return TerminationResult{}, false, xerrors.Errorf("failed to walk early terminations bitfield for deadlines: %w", err)
	}

	// Removed finished partitions from the index.
	for _, finished := range partitionsFinished {
		dl.EarlyTerminations.Unset(finished)
	}

	// Save deadline's partitions
	dl.Partitions, err = partitions.Root()
	if err != nil {
		return TerminationResult{}, false, xerrors.Errorf("failed to update partitions")
	}

	// Update global early terminations bitfield.
	noEarlyTerminations, err := dl.EarlyTerminations.IsEmpty()
	if err != nil {
		return TerminationResult{}, false, xerrors.Errorf("failed to count remaining early terminations partitions: %w", err)
	}

	return result, !noEarlyTerminations, nil
}

// Returns nil if nothing was popped.
func (dl *Deadline) popExpiredPartitions(store adt.Store, until abi.ChainEpoch, quant QuantSpec) (*abi.BitField, bool, error) {
	expirations, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant)
	if err != nil {
		return nil, false, err
	}

	popped, modified, err := expirations.PopUntil(until)
	if err != nil {
		return nil, false, xerrors.Errorf("failed to pop expiring partitions: %w", err)
	}

	if modified {
		dl.ExpirationsEpochs, err = expirations.Root()
		if err != nil {
			return nil, false, err
		}
	}

	return popped, modified, nil
}

func (dl *Deadline) TerminateSectors(
	store adt.Store,
	sectors Sectors,
	epoch abi.ChainEpoch,
	partitionSectors PartitionSectorMap,
	ssize abi.SectorSize,
	quant QuantSpec,
) (powerLost PowerPair, err error) {

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return NewPowerPairZero(), err
	}

	powerLost = NewPowerPairZero()
	var partition Partition
	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos *abi.BitField) error {
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		} else if !found {
			return xc.ErrNotFound.Wrapf("failed to find partition %d", partIdx)
		}

		removed, err := partition.TerminateSectors(store, sectors, epoch, sectorNos, ssize, quant)
		if err != nil {
			return xerrors.Errorf("failed to terminate sectors in partition %d: %w", partIdx, err)
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xerrors.Errorf("failed to store updated partition %d: %w", partIdx, err)
		}

		if count, err := removed.Count(); err != nil {
			return xerrors.Errorf("failed to count terminated sectors in partition %d: %w", partIdx, err)
		} else if count > 0 {
			// Record that partition now has pending early terminations.
			dl.EarlyTerminations.Set(partIdx)
			// Record change to sectors and power
			dl.LiveSectors -= count
		} // note: we should _always_ have early terminations, unless the early termination bitfield is empty.

		dl.FaultyPower = dl.FaultyPower.Sub(removed.FaultyPower)

		// Aggregate power lost from active sectors
		powerLost = powerLost.Add(removed.ActivePower)
		return nil
	}); err != nil {
		return NewPowerPairZero(), err
	}

	// save partitions back
	dl.Partitions, err = partitions.Root()
	if err != nil {
		return NewPowerPairZero(), xerrors.Errorf("failed to persist partitions: %w", err)
	}

	return powerLost, nil
}

// RemovePartitions removes the specified partitions, shifting the remaining
// ones to the left, and returning the live and dead sectors they contained.
//
// Returns an error if any of the partitions contained faulty sectors or early
// terminations.
func (dl *Deadline) RemovePartitions(store adt.Store, toRemove *bitfield.BitField, quant QuantSpec) (
	live, dead *abi.BitField, removedPower PowerPair, err error,
) {
	oldPartitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to load partitions: %w", err)
	}

	partitionCount := oldPartitions.Length()
	toRemoveSet, err := toRemove.AllMap(partitionCount)
	if err != nil {
		return nil, nil, NewPowerPairZero(), xc.ErrIllegalArgument.Wrapf("failed to expand partitions into map: %w", err)
	}

	// Nothing to do.
	if len(toRemoveSet) == 0 {
		return bitfield.NewFromSet(nil), bitfield.NewFromSet(nil), NewPowerPairZero(), nil
	}

	for partIdx := range toRemoveSet { //nolint:nomaprange
		if partIdx >= partitionCount {
			return nil, nil, NewPowerPairZero(), xc.ErrIllegalArgument.Wrapf(
				"partition index %d out of range [0, %d)", partIdx, partitionCount,
			)
		}
	}

	// Should already be checked earlier, but we might as well check again.
	noEarlyTerminations, err := dl.EarlyTerminations.IsEmpty()
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to check for early terminations: %w", err)
	}
	if !noEarlyTerminations {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("cannot remove partitions from deadline with early terminations: %w", err)
	}

	newPartitions := adt.MakeEmptyArray(store)
	allDeadSectors := make([]*bitfield.BitField, 0, len(toRemoveSet))
	allLiveSectors := make([]*bitfield.BitField, 0, len(toRemoveSet))
	removedPower = NewPowerPairZero()

	// Define all of these out here to save allocations.
	var (
		lazyPartition cbg.Deferred
		byteReader    bytes.Reader
		partition     Partition
	)
	if err = oldPartitions.ForEach(&lazyPartition, func(partIdx int64) error {
		// If we're keeping the partition as-is, append it to the new partitions array.
		if _, ok := toRemoveSet[uint64(partIdx)]; !ok {
			return newPartitions.AppendContinuous(&lazyPartition)
		}

		// Ok, actually unmarshal the partition.
		byteReader.Reset(lazyPartition.Raw)
		err := partition.UnmarshalCBOR(&byteReader)
		byteReader.Reset(nil)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to decode partition %d: %w", partIdx, err)
		}

		// Don't allow removing partitions with faulty sectors.
		hasNoFaults, err := partition.Faults.IsEmpty()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to decode faults for partition %d: %w", partIdx, err)
		}
		if !hasNoFaults {
			return xc.ErrIllegalArgument.Wrapf("cannot remove partition %d: has faults", partIdx)
		}

		// Get the live sectors.
		liveSectors, err := partition.LiveSectors()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to calculate live sectors for partition %d: %w", partIdx, err)
		}

		allDeadSectors = append(allDeadSectors, partition.Terminated)
		allLiveSectors = append(allLiveSectors, liveSectors)
		removedPower = removedPower.Add(partition.LivePower)
		return nil
	}); err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("while removing partitions: %w", err)
	}

	dl.Partitions, err = newPartitions.Root()
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to persist new partition table: %w", err)
	}

	dead, err = bitfield.MultiMerge(allDeadSectors...)
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to merge dead sector bitfields: %w", err)
	}
	live, err = bitfield.MultiMerge(allLiveSectors...)
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to merge live sector bitfields: %w", err)
	}

	// Update sector counts.
	removedDeadSectors, err := dead.Count()
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to count dead sectors: %w", err)
	}

	removedLiveSectors, err := live.Count()
	if err != nil {
		return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to count live sectors: %w", err)
	}

	dl.LiveSectors -= removedLiveSectors
	dl.TotalSectors -= removedLiveSectors + removedDeadSectors

	// Update expiration bitfields.
	{
		expirationEpochs, err := LoadBitfieldQueue(store, dl.ExpirationsEpochs, quant)
		if err != nil {
			return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed to load expiration queue: %w", err)
		}

		err = expirationEpochs.Cut(toRemove)
		if err != nil {
			return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed cut removed partitions from deadline expiration queue: %w", err)
		}

		dl.ExpirationsEpochs, err = expirationEpochs.Root()
		if err != nil {
			return nil, nil, NewPowerPairZero(), xerrors.Errorf("failed persist deadline expiration queue: %w", err)
		}
	}

	return live, dead, removedPower, nil
}

func (dl *Deadline) DeclareFaults(
	store adt.Store, sectors Sectors, ssize abi.SectorSize, quant QuantSpec,
	faultExpirationEpoch abi.ChainEpoch, partitionSectors PartitionSectorMap,
) (newFaultyPower PowerPair, err error) {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return NewPowerPairZero(), err
	}

	// Record partitions with some fault, for subsequently indexing in the deadline.
	// Duplicate entries don't matter, they'll be stored in a bitfield (a set).
	partitionsWithFault := make([]uint64, 0, len(partitionSectors))
	newFaultyPower = NewPowerPairZero()
	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos *abi.BitField) error {
		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to load partition %d: %w", partIdx, err)
		}
		if !found {
			return xc.ErrNotFound.Wrapf("no such partition %d", partIdx)
		}

		err = validateFRDeclarationPartition(&partition, sectorNos)
		if err != nil {
			return exitcode.ErrIllegalArgument.Wrapf("failed fault declaration for %d: %w", partIdx, err)
		}

		// Split declarations into declarations of new faults, and retraction of declared recoveries.
		retractedRecoveries, err := bitfield.IntersectBitField(partition.Recoveries, sectorNos)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to intersect sectors with recoveries: %w", err)
		}

		newFaults, err := bitfield.SubtractBitField(sectorNos, retractedRecoveries)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to subtract recoveries from sectors: %w", err)
		}
		// Ignore any terminated sectors and previously declared or detected faults
		newFaults, err = bitfield.SubtractBitField(newFaults, partition.Terminated)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to subtract terminations from faults: %w", err)
		}
		newFaults, err = bitfield.SubtractBitField(newFaults, partition.Faults)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to subtract existing faults from faults: %w", err)
		}

		// Add new faults to state.
		empty, err := newFaults.IsEmpty()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to check if bitfield was empty: %w", err)
		}
		if !empty {
			newFaultSectors, err := sectors.Load(newFaults)
			if err != nil {
				return xc.ErrIllegalState.Wrapf("failed to load fault sectors: %w", err)
			}

			newPartitionFaultyPower, err := partition.AddFaults(store, newFaults, newFaultSectors, faultExpirationEpoch, ssize, quant)
			if err != nil {
				return xc.ErrIllegalState.Wrapf("failed to add faults: %w", err)
			}

			newFaultyPower = newFaultyPower.Add(newPartitionFaultyPower)
			partitionsWithFault = append(partitionsWithFault, partIdx)
		}

		// Remove faulty recoveries from state.
		empty, err = retractedRecoveries.IsEmpty()
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to check if bitfield was empty: %w", err)
		}
		if !empty {
			retractedRecoverySectors, err := sectors.Load(retractedRecoveries)
			if err != nil {
				return xc.ErrIllegalState.Wrapf("failed to load recovery sectors: %w", err)
			}
			retractedRecoveryPower := PowerForSectors(ssize, retractedRecoverySectors)

			err = partition.RemoveRecoveries(retractedRecoveries, retractedRecoveryPower)
			if err != nil {
				return xc.ErrIllegalState.Wrapf("failed to remove recoveries: %w", err)
			}
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to store partition %d: %w", partIdx, err)
		}

		return nil
	}); err != nil {
		return NewPowerPairZero(), err
	}

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return NewPowerPairZero(), xc.ErrIllegalState.Wrapf("failed to store partitions root: %w", err)
	}

	err = dl.AddExpirationPartitions(store, faultExpirationEpoch, partitionsWithFault, quant)
	if err != nil {
		return NewPowerPairZero(), xc.ErrIllegalState.Wrapf("failed to update expirations for partitions with faults: %w", err)
	}

	dl.FaultyPower = dl.FaultyPower.Add(newFaultyPower)

	return newFaultyPower, nil
}

func (dl *Deadline) DeclareFaultsRecovered(
	store adt.Store, sectors Sectors, ssize abi.SectorSize,
	partitionSectors PartitionSectorMap,
) (err error) {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return err
	}

	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos *abi.BitField) error {
		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to load partition %d: %w", partIdx, err)
		}
		if !found {
			return xc.ErrNotFound.Wrapf("no such partition %d", partIdx)
		}

		err = validateFRDeclarationPartition(&partition, sectorNos)
		if err != nil {
			return exitcode.ErrIllegalArgument.Wrapf("failed fault declaration for %d: %w", partIdx, err)
		}

		// Ignore sectors not faulty or already declared recovered
		recoveries, err := bitfield.IntersectBitField(sectorNos, partition.Faults)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to intersect recoveries with faults: %w", err)
		}
		recoveries, err = bitfield.SubtractBitField(recoveries, partition.Recoveries)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to subtract existing recoveries: %w", err)
		}

		// Record the new recoveries for processing at Window PoSt or deadline cron.
		recoverySectors, err := sectors.Load(recoveries)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to load recovery sectors: %w", err)
		}
		recoveryPower := PowerForSectors(ssize, recoverySectors)

		err = partition.AddRecoveries(recoveries, recoveryPower)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to add recoveries: %w", err)
		}

		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return xc.ErrIllegalState.Wrapf("failed to update partition %d: %w", partIdx, err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Power is not regained until the deadline end, when the recovery is confirmed.

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return xc.ErrIllegalState.Wrapf("failed to store partitions root: %w", err)
	}
	return nil
}

// ProcessDeadlineEnd processes all PoSt submissions, marking unproven sectors as
// faulty and clearing failed recoveries. It returns any new faulty power and
// failed recovery power.
func (dl *Deadline) ProcessDeadlineEnd(store adt.Store, quant QuantSpec, faultExpirationEpoch abi.ChainEpoch) (
	newFaultyPower, failedRecoveryPower PowerPair, err error,
) {
	newFaultyPower = NewPowerPairZero()
	failedRecoveryPower = NewPowerPairZero()

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to load partitions: %w", err)
	}

	detectedAny := false
	var rescheduledPartitions []uint64
	for partIdx := uint64(0); partIdx < partitions.Length(); partIdx++ {
		proven, err := dl.PostSubmissions.IsSet(partIdx)
		if err != nil {
			return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to check submission for partition %d: %w", partIdx, err)
		}
		if proven {
			continue
		}

		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to load partition %d: %w", partIdx, err)
		}
		if !found {
			return newFaultyPower, failedRecoveryPower, exitcode.ErrIllegalState.Wrapf("no partition %d", partIdx)
		}

		// If we have no recovering power/sectors, and all power is faulty, skip
		// this. This lets us skip some work if a miner repeatedly fails to PoSt.
		if partition.RecoveringPower.IsZero() && partition.FaultyPower.Equals(partition.LivePower) {
			continue
		}

		// Ok, we actually need to process this partition. Make sure we save the partition state back.
		detectedAny = true

		partFaultyPower, partFailedRecoveryPower, err := partition.RecordMissedPost(store, faultExpirationEpoch, quant)
		if err != nil {
			return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to record missed PoSt for partition %v: %w", partIdx, err)
		}

		// We marked some sectors faulty, we need to record the new
		// expiration. We don't want to do this if we're just penalizing
		// the miner for failing to recover power.
		if !partFaultyPower.IsZero() {
			rescheduledPartitions = append(rescheduledPartitions, partIdx)
		}

		// Save new partition state.
		err = partitions.Set(partIdx, &partition)
		if err != nil {
			return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to update partition %v: %w", partIdx, err)
		}

		newFaultyPower = newFaultyPower.Add(partFaultyPower)
		failedRecoveryPower = failedRecoveryPower.Add(partFailedRecoveryPower)
	}

	// Save modified deadline state.
	if detectedAny {
		dl.Partitions, err = partitions.Root()
		if err != nil {
			return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to store partitions: %w", err)
		}
	}

	err = dl.AddExpirationPartitions(store, faultExpirationEpoch, rescheduledPartitions, quant)
	if err != nil {
		return newFaultyPower, failedRecoveryPower, xc.ErrIllegalState.Wrapf("failed to update deadline expiration queue: %w", err)
	}

	dl.FaultyPower = dl.FaultyPower.Add(newFaultyPower)

	// Reset PoSt submissions.
	dl.PostSubmissions = abi.NewBitField()
	return newFaultyPower, failedRecoveryPower, nil
}

type PoStResult struct {
	NewFaultyPower, RetractedRecoveryPower, RecoveredPower PowerPair
	// Sectors is a bitfield of all sectors in the proven partitions.
	Sectors *bitfield.BitField
	// IgnoredSectors is a subset of Sectors that should be ignored.
	IgnoredSectors *bitfield.BitField
}

// PowerDelta returns the power change (positive or negative) after processing
// the PoSt submission.
func (p *PoStResult) PowerDelta() PowerPair {
	return p.RecoveredPower.Sub(p.NewFaultyPower)
}

// PenaltyPower is the power from this PoSt that should be penalized.
func (p *PoStResult) PenaltyPower() PowerPair {
	return p.NewFaultyPower.Add(p.RetractedRecoveryPower)
}

// RecordProvenSectors processes a series of posts, recording proven partitions
// and marking skipped sectors as faulty.
//
// It returns a PoStResult containing the list of proven and skipped sectors and
// changes to power (newly faulty power, power that should have been proven
// recovered but wasn't, and newly recovered power).
//
// NOTE: This function does not actually _verify_ any proofs. The returned
// Sectors and IgnoredSectors must subsequently be validated against the PoSt
// submitted by the miner.
func (dl *Deadline) RecordProvenSectors(
	store adt.Store, sectors Sectors,
	ssize abi.SectorSize, quant QuantSpec, faultExpiration abi.ChainEpoch,
	postPartitions []PoStPartition,
) (*PoStResult, error) {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, err
	}

	allSectors := make([]*abi.BitField, 0, len(postPartitions))
	allIgnored := make([]*abi.BitField, 0, len(postPartitions))
	newFaultyPowerTotal := NewPowerPairZero()
	retractedRecoveryPowerTotal := NewPowerPairZero()
	recoveredPowerTotal := NewPowerPairZero()
	var rescheduledPartitions []uint64

	// Accumulate sectors info for proof verification.
	for _, post := range postPartitions {
		alreadyProven, err := dl.PostSubmissions.IsSet(post.Index)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to check if partition %d already posted: %w", post.Index, err)
		}
		if alreadyProven {
			// Skip partitions already proven for this deadline.
			continue
		}

		var partition Partition
		found, err := partitions.Get(post.Index, &partition)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to load partition d: %w", post.Index, err)
		} else if !found {
			return nil, exitcode.ErrNotFound.Wrapf("no such partition %d", post.Index)
		}

		// Process new faults and accumulate new faulty power.
		// This updates the faults in partition state ahead of calculating the sectors to include for proof.
		newFaultPower, retractedRecoveryPower, err := partition.RecordSkippedFaults(
			store, sectors, ssize, quant, faultExpiration, post.Skipped,
		)
		if err != nil {
			return nil, xerrors.Errorf("failed to add skipped faults to partition %d: %w", post.Index, err)
		}

		// If we have new faulty power, we've added some faults. We need
		// to record the new expiration in the deadline.
		if !newFaultPower.IsZero() {
			rescheduledPartitions = append(rescheduledPartitions, post.Index)
		}

		// Process recoveries, assuming the proof will be successful.
		// This similarly updates state.
		recoveredSectors, err := sectors.Load(partition.Recoveries)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to load recovered sectors for partition %d: %w", post.Index, err)
		}

		recoveredPower, err := partition.RecoverFaults(store, partition.Recoveries, recoveredSectors, ssize, quant)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to remove recoveries from faults for partition %d: %w", post.Index, err)
		}

		// This will be rolled back if the method aborts with a failed proof.
		err = partitions.Set(post.Index, &partition)
		if err != nil {
			return nil, xc.ErrIllegalState.Wrapf("failed to update partition %v: %w", post.Index, err)
		}

		newFaultyPowerTotal = newFaultyPowerTotal.Add(newFaultPower)
		retractedRecoveryPowerTotal = retractedRecoveryPowerTotal.Add(retractedRecoveryPower)
		recoveredPowerTotal = recoveredPowerTotal.Add(recoveredPower)

		// Record the post.
		dl.PostSubmissions.Set(post.Index)

		// At this point, the partition faults represents the expected faults for the proof, with new skipped
		// faults and recoveries taken into account.
		allSectors = append(allSectors, partition.Sectors)
		allIgnored = append(allIgnored, partition.Faults)
		allIgnored = append(allIgnored, partition.Terminated)
	}

	err = dl.AddExpirationPartitions(store, faultExpiration, rescheduledPartitions, quant)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to update expirations for partitions with faults: %w", err)
	}

	// Save everything back.
	dl.FaultyPower = dl.FaultyPower.Sub(recoveredPowerTotal).Add(newFaultyPowerTotal)

	dl.Partitions, err = partitions.Root()
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to persist partitions: %w", err)
	}

	// Collect all sectors, faults, and recoveries for proof verification.
	allSectorNos, err := bitfield.MultiMerge(allSectors...)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to merge all sectors bitfields: %w", err)
	}
	allIgnoredSectorNos, err := bitfield.MultiMerge(allIgnored...)
	if err != nil {
		return nil, xc.ErrIllegalState.Wrapf("failed to merge ignored sectors bitfields: %w", err)
	}

	return &PoStResult{
		Sectors:                allSectorNos,
		IgnoredSectors:         allIgnoredSectorNos,
		NewFaultyPower:         newFaultyPowerTotal,
		RecoveredPower:         recoveredPowerTotal,
		RetractedRecoveryPower: retractedRecoveryPowerTotal,
	}, nil
}

// RescheduleSectorExpirations reschedules the expirations of the given sectors
// to the target epoch, skipping any sectors it can't find.
//
// The power of the rescheduled sectors is assumed to have not changed since
// initial scheduling.
//
// Note: see the docs on State.RescheduleSectorExpirations for details on why we
// skip sectors/partitions we can't find.
func (dl *Deadline) RescheduleSectorExpirations(
	store adt.Store, sectors Sectors,
	expiration abi.ChainEpoch, partitionSectors PartitionSectorMap,
	ssize abi.SectorSize, quant QuantSpec,
) error {
	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return err
	}

	var rescheduledPartitions []uint64 // track partitions with moved expirations.
	if err := partitionSectors.ForEach(func(partIdx uint64, sectorNos *abi.BitField) error {
		var partition Partition
		if found, err := partitions.Get(partIdx, &partition); err != nil {
			return xerrors.Errorf("failed to load partition %d: %w", partIdx, err)
		} else if !found {
			// We failed to find the partition, it could have moved
			// due to compaction. This function is only reschedules
			// sectors it can find so we'll just skip it.
			return nil
		}

		moved, err := partition.RescheduleExpirations(store, sectors, expiration, sectorNos, ssize, quant)
		if err != nil {
			return xerrors.Errorf("failed to reschedule expirations in partition %d: %w", partIdx, err)
		}
		if empty, err := moved.IsEmpty(); err != nil {
			return xerrors.Errorf("failed to parse bitfield of rescheduled expirations: %w", err)
		} else if empty {
			// nothing moved.
			return nil
		}

		rescheduledPartitions = append(rescheduledPartitions, partIdx)
		if err = partitions.Set(partIdx, &partition); err != nil {
			return xerrors.Errorf("failed to store partition %d: %w", partIdx, err)
		}
		return nil
	}); err != nil {
		return err
	}

	if len(rescheduledPartitions) > 0 {
		dl.Partitions, err = partitions.Root()
		if err != nil {
			return xerrors.Errorf("failed to save partitions: %w", err)
		}
		err := dl.AddExpirationPartitions(store, expiration, rescheduledPartitions, quant)
		if err != nil {
			return xerrors.Errorf("failed to reschedule partition expirations: %w", err)
		}
	}

	return nil
}
