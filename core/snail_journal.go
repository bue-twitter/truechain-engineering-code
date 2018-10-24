// Copyright 2017 The truechian Authors
// This file is part of the truechain library.
//
// The truechain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The truechain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the truechain library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"io"
	"os"

	"github.com/truechain/truechain-engineering-code/common"
	"github.com/truechain/truechain-engineering-code/core/types"
	"github.com/truechain/truechain-engineering-code/log"
	"github.com/truechain/truechain-engineering-code/rlp"
)

// errNoActiveSnailJournal is returned if a fruit is attempted to be inserted
// into the journal, but no such file is currently open.
var errNoActiveSnailJournal = errors.New("no active snail journal")

// snailDevNull is a WriteCloser that just discards anything written into it. Its
// goal is to allow the snail journal to write into a fake journal when
// loading fruits on startup without printing warnings due to no file
// being readt for write.
type snailDevNull struct{}

func (*snailDevNull) Write(p []byte) (n int, err error) { return len(p), nil }
func (*snailDevNull) Close() error                      { return nil }

// snailJournal is a rotating log of fruits with the aim of storing locally
// created fruits to allow non-executed ones to survive node restarts.
type snailJournal struct {
	path   string         // Filesystem path to store the fruits at
	writer io.WriteCloser // Output stream to write new fruits into
}

// newsnailJournal creates a new fruit journal to
func newSnailJournal(path string) *snailJournal {
	return &snailJournal{
		path: path,
	}
}

// load parses a fruit journal dump from disk, loading its contents into
// the specified pool.
func (journal *snailJournal) load(add func([]*types.SnailBlock) []error) error {
	// Skip the parsing if the journal file doens't exist at all
	if _, err := os.Stat(journal.path); os.IsNotExist(err) {
		return nil
	}
	// Open the journal for loading any past fruits
	input, err := os.Open(journal.path)
	if err != nil {
		return err
	}
	defer input.Close()

	// Temporarily discard any journal additions (don't double add on load)
	journal.writer = new(snailDevNull)
	defer func() { journal.writer = nil }()

	// Inject all fruits from the journal into the pool
	stream := rlp.NewStream(input, 0)
	total, dropped := 0, 0

	// Create a method to load a limited batch of fruits and bump the
	// appropriate progress counters. Then use this method to load all the
	// journalled fruits in small-ish batches.
	loadBatch := func(fruits types.Fruits) {
		for _, err := range add(fruits) {
			if err != nil {
				log.Debug("Failed to add journaled fruit", "err", err)
				dropped++
			}
		}
	}
	var (
		failure error
		batch   types.Fruits
	)
	for {
		// Parse the next fruit and terminate on error
		fruit := new(types.SnailBlock)
		if err = stream.Decode(fruit); err != nil {
			if err != io.EOF {
				failure = err
			}
			if batch.Len() > 0 {
				loadBatch(batch)
			}
			break
		}
		// New fruit parsed, queue up for later, import if threnshold is reached
		total++

		if batch = append(batch, fruit); batch.Len() > 1024 {
			loadBatch(batch)
			batch = batch[:0]
		}
	}
	log.Info("Loaded local fruit journal", "fruits", total, "dropped", dropped)

	return failure
}

// insert adds the specified fruit to the local disk journal.
func (journal *snailJournal) insert(fruit *types.SnailBlock) error {
	if journal.writer == nil {
		return errNoActiveSnailJournal
	}
	if err := rlp.Encode(journal.writer, fruit); err != nil {
		return err
	}
	return nil
}

// rotate regenerates the fruit journal based on the current contents of
// the fruit pool.
func (journal *snailJournal) rotate(all map[common.Address]types.Fruits) error {
	// Close the current journal (if any is open)
	if journal.writer != nil {
		if err := journal.writer.Close(); err != nil {
			return err
		}
		journal.writer = nil
	}
	// Generate a new journal with the contents of the current pool
	replacement, err := os.OpenFile(journal.path+".new", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	journaled := 0
	for _, fruits := range all {
		for _, fruit := range fruits {
			if err = rlp.Encode(replacement, fruit); err != nil {
				replacement.Close()
				return err
			}
		}
		journaled += len(fruits)
	}
	replacement.Close()

	// Replace the live journal with the newly generated one
	if err = os.Rename(journal.path+".new", journal.path); err != nil {
		return err
	}
	sink, err := os.OpenFile(journal.path, os.O_WRONLY|os.O_APPEND, 0755)
	if err != nil {
		return err
	}
	journal.writer = sink
	log.Info("Regenerated local fruit journal", "fruits", journaled, "accounts", len(all))

	return nil
}

// close flushes the fruit journal contents to disk and closes the file.
func (journal *snailJournal) close() error {
	var err error

	if journal.writer != nil {
		err = journal.writer.Close()
		journal.writer = nil
	}
	return err
}