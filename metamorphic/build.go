// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package metamorphic

import (
	"context"
	"fmt"
	"slices"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/private"
	"github.com/cockroachdb/pebble/internal/rangekey"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
)

// writeSSTForIngestion writes an SST that is to be ingested, either directly or
// as an external file. Returns the sstable metadata.
//
// Closes the iterators in all cases.
func writeSSTForIngestion(
	t *Test,
	pointIter base.InternalIterator,
	rangeDelIter keyspan.FragmentIterator,
	rangeKeyIter keyspan.FragmentIterator,
	uniquePrefixes bool,
	syntheticSuffix sstable.SyntheticSuffix,
	syntheticPrefix sstable.SyntheticPrefix,
	writable objstorage.Writable,
	targetFMV pebble.FormatMajorVersion,
) (*sstable.WriterMetadata, error) {
	writerOpts := t.opts.MakeWriterOptions(0, targetFMV.MaxTableFormat())
	if t.testOpts.disableValueBlocksForIngestSSTables {
		writerOpts.DisableValueBlocks = true
	}
	w := sstable.NewWriter(writable, writerOpts)
	pointIterCloser := base.CloseHelper(pointIter)
	defer pointIterCloser.Close()
	rangeDelIterCloser := base.CloseHelper(rangeDelIter)
	defer rangeDelIterCloser.Close()
	rangeKeyIterCloser := base.CloseHelper(rangeKeyIter)
	defer rangeKeyIterCloser.Close()

	outputKey := func(key []byte) []byte {
		if !syntheticPrefix.IsSet() && !syntheticSuffix.IsSet() {
			return slices.Clone(key)
		}
		if syntheticPrefix.IsSet() {
			key = syntheticPrefix.Apply(key)
		}
		if syntheticSuffix.IsSet() {
			n := t.opts.Comparer.Split(key)
			key = append(key[:n:n], syntheticSuffix...)
		}
		return key
	}

	var lastUserKey []byte
	for key, value := pointIter.First(); key != nil; key, value = pointIter.Next() {
		// Ignore duplicate keys.
		if lastUserKey != nil {
			last := lastUserKey
			this := key.UserKey
			if uniquePrefixes {
				last = last[:t.opts.Comparer.Split(last)]
				this = this[:t.opts.Comparer.Split(this)]
			}
			if t.opts.Comparer.Equal(last, this) {
				continue
			}
		}
		lastUserKey = append(lastUserKey[:0], key.UserKey...)

		key.SetSeqNum(base.SeqNumZero)
		// It's possible that we wrote the key on a batch from a db that supported
		// DeleteSized, but will be ingesting into a db that does not. Detect this
		// case and translate the key to an InternalKeyKindDelete.
		if targetFMV < pebble.FormatDeleteSizedAndObsolete && key.Kind() == pebble.InternalKeyKindDeleteSized {
			value = pebble.LazyValue{}
			key.SetKind(pebble.InternalKeyKindDelete)
		}
		valBytes, _, err := value.Value(nil)
		if err != nil {
			return nil, err
		}
		k := *key
		k.UserKey = outputKey(k.UserKey)
		if err := w.Add(k, valBytes); err != nil {
			return nil, err
		}
	}
	if err := pointIterCloser.Close(); err != nil {
		return nil, err
	}

	if rangeDelIter != nil {
		span, err := rangeDelIter.First()
		for ; span != nil; span, err = rangeDelIter.Next() {
			if err := w.DeleteRange(outputKey(span.Start), outputKey(span.End)); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
		if err := rangeDelIterCloser.Close(); err != nil {
			return nil, err
		}
	}

	if rangeKeyIter != nil {
		span, err := rangeKeyIter.First()
		for ; span != nil; span, err = rangeKeyIter.Next() {
			// Coalesce the keys of this span and then zero the sequence
			// numbers. This is necessary in order to make the range keys within
			// the ingested sstable internally consistent at the sequence number
			// it's ingested at. The individual keys within a batch are
			// committed at unique sequence numbers, whereas all the keys of an
			// ingested sstable are given the same sequence number. A span
			// containing keys that both set and unset the same suffix at the
			// same sequence number is nonsensical, so we "coalesce" or collapse
			// the keys.
			collapsed := keyspan.Span{
				Start: outputKey(span.Start),
				End:   outputKey(span.End),
				Keys:  make([]keyspan.Key, 0, len(span.Keys)),
			}
			rangekey.Coalesce(
				t.opts.Comparer.Compare, t.opts.Comparer.Equal, span.Keys, &collapsed.Keys,
			)
			for i := range collapsed.Keys {
				collapsed.Keys[i].Trailer = base.MakeTrailer(0, collapsed.Keys[i].Kind())
			}
			keyspan.SortKeysByTrailer(&collapsed.Keys)
			if err := rangekey.Encode(&collapsed, w.AddRangeKey); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, err
		}
		if err := rangeKeyIterCloser.Close(); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	sstMeta, err := w.Metadata()
	if err != nil {
		return nil, err
	}
	return sstMeta, nil
}

// buildForIngest builds a local SST file containing the keys in the given batch
// and returns its path and metadata.
func buildForIngest(
	t *Test, dbID objID, b *pebble.Batch, i int,
) (path string, _ *sstable.WriterMetadata, _ error) {
	path = t.opts.FS.PathJoin(t.tmpDir, fmt.Sprintf("ext%d-%d", dbID.slot(), i))
	f, err := t.opts.FS.Create(path)
	if err != nil {
		return "", nil, err
	}
	db := t.getDB(dbID)

	iter, rangeDelIter, rangeKeyIter := private.BatchSort(b)

	writable := objstorageprovider.NewFileWritable(f)
	meta, err := writeSSTForIngestion(
		t,
		iter, rangeDelIter, rangeKeyIter,
		false, /* uniquePrefixes */
		nil,   /* syntheticSuffix */
		nil,   /* syntheticPrefix */
		writable,
		db.FormatMajorVersion(),
	)
	return path, meta, err
}

// buildForIngest builds a local SST file containing the keys in the given
// external object (truncated to the given bounds) and returns its path and
// metadata.
func buildForIngestExternalEmulation(
	t *Test,
	dbID objID,
	externalObjID objID,
	bounds pebble.KeyRange,
	syntheticSuffix sstable.SyntheticSuffix,
	syntheticPrefix sstable.SyntheticPrefix,
	i int,
) (path string, _ *sstable.WriterMetadata) {
	path = t.opts.FS.PathJoin(t.tmpDir, fmt.Sprintf("ext%d-%d", dbID.slot(), i))
	f, err := t.opts.FS.Create(path)
	panicIfErr(err)

	reader, pointIter, rangeDelIter, rangeKeyIter := openExternalObj(t, externalObjID, bounds, syntheticPrefix)
	defer reader.Close()

	writable := objstorageprovider.NewFileWritable(f)
	// The underlying file should already have unique prefixes. Plus we are
	// emulating the external ingestion path which won't remove duplicate prefixes
	// if they exist.
	const uniquePrefixes = false
	meta, err := writeSSTForIngestion(
		t,
		pointIter, rangeDelIter, rangeKeyIter,
		uniquePrefixes,
		syntheticSuffix,
		syntheticPrefix,
		writable,
		t.minFMV(),
	)
	if err != nil {
		panic(err)
	}
	return path, meta
}

func openExternalObj(
	t *Test, externalObjID objID, bounds pebble.KeyRange, syntheticPrefix sstable.SyntheticPrefix,
) (
	reader *sstable.Reader,
	pointIter base.InternalIterator,
	rangeDelIter keyspan.FragmentIterator,
	rangeKeyIter keyspan.FragmentIterator,
) {
	objReader, objSize, err := t.externalStorage.ReadObject(context.Background(), externalObjName(externalObjID))
	panicIfErr(err)
	opts := sstable.ReaderOptions{
		Comparer: t.opts.Comparer,
	}
	reader, err = sstable.NewReader(objstorageprovider.NewRemoteReadable(objReader, objSize), opts)
	panicIfErr(err)

	start := bounds.Start
	end := bounds.End
	if syntheticPrefix.IsSet() {
		start = syntheticPrefix.Invert(start)
		end = syntheticPrefix.Invert(end)
	}
	pointIter, err = reader.NewIter(sstable.NoTransforms, start, end)
	panicIfErr(err)

	rangeDelIter, err = reader.NewRawRangeDelIter(sstable.NoTransforms)
	panicIfErr(err)
	if rangeDelIter != nil {
		rangeDelIter = keyspan.Truncate(
			t.opts.Comparer.Compare,
			rangeDelIter,
			start, end,
			nil /* start */, nil /* end */, false, /* panicOnUpperTruncate */
		)
	}

	rangeKeyIter, err = reader.NewRawRangeKeyIter(sstable.NoTransforms)
	panicIfErr(err)
	if rangeKeyIter != nil {
		rangeKeyIter = keyspan.Truncate(
			t.opts.Comparer.Compare,
			rangeKeyIter,
			start, end,
			nil /* start */, nil /* end */, false, /* panicOnUpperTruncate */
		)
	}
	return reader, pointIter, rangeDelIter, rangeKeyIter
}

// externalObjIsEmpty returns true if the given external object has no point or
// range keys withing the given bounds.
func externalObjIsEmpty(
	t *Test, externalObjID objID, bounds pebble.KeyRange, syntheticPrefix sstable.SyntheticPrefix,
) bool {
	reader, pointIter, rangeDelIter, rangeKeyIter := openExternalObj(t, externalObjID, bounds, syntheticPrefix)
	defer reader.Close()
	defer closeIters(pointIter, rangeDelIter, rangeKeyIter)

	key, _ := pointIter.First()
	panicIfErr(pointIter.Error())
	if key != nil {
		return false
	}
	for _, it := range []keyspan.FragmentIterator{rangeDelIter, rangeKeyIter} {
		if it != nil {
			span, err := it.First()
			panicIfErr(err)
			if span != nil {
				return false
			}
		}
	}
	return true
}

func panicIfErr(err error) {
	if err != nil {
		panic(err)
	}
}
