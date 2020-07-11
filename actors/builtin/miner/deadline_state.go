package miner

import (
	"fmt"
	"io"
	"sort"

	"github.com/filecoin-project/go-bitfield"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
)

// A bitfield of sector numbers due at each deadline.
// The sectors for each deadline are logically grouped into sequential partitions for proving.
type Deadlines struct {
	// TODO: Consider inlining part of the deadline struct (e.g.,
	// active/assigned sectors) to make sector assignment cheaper. At the
	// moment, assigning a sector requires loading all deadlines to figure
	// out where best to assign new sectors.
	Due [WPoStPeriodDeadlines]cid.Cid // []Deadline
}

type Deadline struct {
	// Partitions in this deadline, in order.
	// The keys of this AMT are always sequential integers beginning with zero.
	Partitions cid.Cid // AMT[PartitionNumber]Partition

	// Maps epochs to partitions with sectors that became faulty during that epoch.
	FaultsEpochs cid.Cid // AMT[ChainEpoch]BitField

	// Maps epochs to partitions with sectors that expire in that epoch.
	ExpirationsEpochs cid.Cid // AMT[ChainEpoch]BitField

	// Partitions numbers with PoSt submissions since the proving period started.
	PostSubmissions *abi.BitField

	// The number of non-terminated sectors in this deadline.
	LiveSectors uint64
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
		return nil, xerrors.Errorf("invalid deadline %d", dlIdx)
	}
	deadline := new(Deadline)
	err := store.Get(store.Context(), d.Due[dlIdx], deadline)
	if err != nil {
		return nil, err
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
		FaultsEpochs:      emptyArrayCid,
		ExpirationsEpochs: emptyArrayCid,
		PostSubmissions:   abi.NewBitField(),
	}
}

func (d *Deadline) PartitionsArray(store adt.Store) (*adt.Array, error) {
	return adt.AsArray(store, d.Partitions)
}

func (d *Deadline) LoadPartition(store adt.Store, partIdx uint64) (*Partition, error) {
	partitions, err := d.PartitionsArray(store)
	if err != nil {
		return nil, err
	}
	var partition Partition
	found, err := partitions.Get(partIdx, &partition)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, xerrors.Errorf("no partition %d", partIdx)
	}
	return &partition, nil
}

// Adds some partition numbers to the set with faults at an epoch.
func (d *Deadline) AddFaultEpochPartitions(store adt.Store, epoch abi.ChainEpoch, partitions ...uint64) error {
	queue, err := loadEpochQueue(store, d.FaultsEpochs)
	if err != nil {
		return xerrors.Errorf("failed to load fault epoch partitions: %w", err)
	}
	if err := queue.AddToQueueValues(epoch, partitions...); err != nil {
		return xerrors.Errorf("failed to mutate deadline fault epochs: %w", err)
	}
	d.FaultsEpochs, err = queue.Root()
	return nil
}

func (dl *Deadline) PopExpiredPartitions(store adt.Store, until abi.ChainEpoch) (*abi.BitField, error) {
	stopErr := fmt.Errorf("stop")

	partitionExpirationQ, err := adt.AsArray(store, dl.ExpirationsEpochs)
	if err != nil {
		return nil, err
	}

	partitionsWithExpiredSectors := abi.NewBitField()
	var expiredEpochs []uint64

	var bf abi.BitField
	err = partitionExpirationQ.ForEach(&bf, func(i int64) error {
		if abi.ChainEpoch(i) > until {
			return stopErr
		}
		expiredEpochs = append(expiredEpochs, uint64(i))
		partitionsWithExpiredSectors, err = bitfield.MergeBitFields(partitionsWithExpiredSectors, &bf)
		return err
	})
	switch err {
	case nil, stopErr:
	default:
		return nil, err
	}

	// Nothing expired.
	if len(expiredEpochs) == 0 {
		return nil, nil
	}

	err = partitionExpirationQ.BatchDelete(expiredEpochs)
	if err == nil {
		return nil, err
	}

	dl.ExpirationsEpochs, err = partitionExpirationQ.Root()
	if err != nil {
		return nil, err
	}

	return partitionsWithExpiredSectors, nil
}

// TODO: Change this to ForEachExpired... to avoid creating a bitfield that's too large.
// TODO: This must update the partitions' state with Terminate bits, updated power total etc
func (dl *Deadline) PopExpiredSectors(store adt.Store, until abi.ChainEpoch) (*ExpirationSet, error) {
	partitionExpiration, err := dl.PopExpiredPartitions(store, until)
	if err != nil {
		return nil, err
	} else if partitionExpiration == nil {
		// nothing to do.
		return nil, nil
	}

	partitions, err := dl.PartitionsArray(store)
	if err != nil {
		return nil, err
	}

	totalPower := NewPowerPairZero()

	// For each partition with an expired sector, collect the
	// expired sectors and remove them from the queues.
	var expiredSectors []*abi.BitField
	err = partitionExpiration.ForEach(func(partIdx uint64) error {
		var partition Partition
		found, err := partitions.Get(partIdx, &partition)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("missing an expected partition")
		}
		partitionExpiredSectors, err := partition.PopExpiredSectors(store, until)
		if err != nil {
			return err
		}
		expiredSectors = append(expiredSectors, partitionExpiredSectors.OnTimeSectors)
		totalPower = totalPower.Add(partitionExpiredSectors.ActivePower)
		return partitions.Set(partIdx, &partition)
	})
	if err != nil {
		return nil, err
	}
	dl.Partitions, err = partitions.Root()
	if err != nil {
		return nil, err
	}

	allExpiries, err := bitfield.MultiMerge(expiredSectors...)
	if err != nil {
		return nil, err
	}

	// Update live sector count.
	nExpired, err := allExpiries.Count()
	if err != nil {
		return nil, xerrors.Errorf("failed to count expired sectors: %w", err)
	}
	dl.LiveSectors -= nExpired

	return &ExpirationSet{
		OnTimeSectors: allExpiries,
		ActivePower:   totalPower,
	}, nil
}

// Adds sectors to a deadline. It's the caller's responsibility to make sure
// that this deadline isn't currently "open" (i.e., being proved at this point
// in time).
func (dl *Deadline) AddSectors(
	store adt.Store,
	partitionSize uint64,
	sectorSize abi.SectorSize,
	sectors []*SectorOnChainInfo,
) error {
	// TODO: This function is ridiculous. We should try to break it into
	// smaller pieces.

	if len(sectors) == 0 {
		return nil
	}

	partitionDeadlineUpdates := make(map[abi.ChainEpoch][]uint64)

	// First update partitions
	{

		partitions, err := dl.PartitionsArray(store)
		if err != nil {
			return err
		}

		partIdx := partitions.Length()
		if partIdx > 0 {
			partIdx -= 1 // try filling up the last partition first.
		}

		for ; len(sectors) > 0; partIdx++ {

			//
			// Get/create partition to update.
			//

			partition := new(Partition)
			if found, err := partitions.Get(partIdx, partition); err != nil {
				return err
			} else if !found {
				// Calling this once per loop is fine. We're almost
				// never going to add more than one partition per call.
				emptyArray, err := adt.MakeEmptyArray(store).Root()
				if err != nil {
					return err
				}

				partition = ConstructPartition(emptyArray)
			}

			//
			// Figure out which (if any) sectors we want to add to
			// this partition.
			//

			// See if there's room in this partition for more sectors.
			sectorCount, err := partition.Sectors.Count()
			if sectorCount >= partitionSize {
				continue
			}

			size := partitionSize - sectorCount
			if uint64(len(sectors)) < size {
				size = uint64(len(sectors))
			}

			partitionSectors := sectors[:size]
			sectors = sectors[size:]

			//
			// Add sectors to partition.
			//

			err = partition.AddSectors(store, partitionSectors, sectorSize)
			if err != nil {
				return err
			}

			//
			// Save partition back.
			//

			err = partitions.Set(partIdx, partition)
			if err != nil {
				return err
			}

			//
			// Record deadline -> partition mapping so we can later update the deadlines.
			//

			for _, sector := range partitionSectors {
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
			return err
		}
	}

	// Next, update the per-deadline expiration queues.

	{
		deadlineExpirations, err := loadEpochQueue(store, dl.ExpirationsEpochs)
		if err != nil {
			return xerrors.Errorf("failed to load expiration epochs: %w", err)
		}

		// Update each epoch in-order to be deterministic.
		updatedEpochs := make([]abi.ChainEpoch, 0, len(partitionDeadlineUpdates))
		for epoch := range partitionDeadlineUpdates {
			updatedEpochs = append(updatedEpochs, epoch)
		}
		sort.Slice(updatedEpochs, func(i, j int) bool {
			return updatedEpochs[i] < updatedEpochs[j]
		})

		for _, epoch := range updatedEpochs {
			deadlineExpirations.AddToQueueValues(epoch, partitionDeadlineUpdates[epoch]...)
		}

		dl.ExpirationsEpochs, err = deadlineExpirations.Root()
		if err != nil {
			return err
		}
	}

	dl.LiveSectors += uint64(len(sectors))

	return nil
}

func (dl *Deadline) MarshalCBOR(w io.Writer) error {
	panic("implement me")
}

func (dl *Deadline) UnmarshalCBOR(r io.Reader) error {
	panic("implement me")
}