// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package metamorphic

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/objstorage/remote"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/wal"
	"golang.org/x/exp/rand"
)

const (
	minimumFormatMajorVersion = pebble.FormatMinSupported
	// The format major version to use in the default options configurations. We
	// default to the minimum supported format so we exercise the runtime version
	// ratcheting that a cluster upgrading would experience. The randomized
	// options may still use format major versions that are less than
	// defaultFormatMajorVersion but are at least minimumFormatMajorVersion.
	defaultFormatMajorVersion = pebble.FormatMinSupported
	// newestFormatMajorVersionToTest is the most recent format major version
	// the metamorphic tests should use. This may be greater than
	// pebble.FormatNewest when some format major versions are marked as
	// experimental.
	newestFormatMajorVersionToTest = pebble.FormatNewest
)

func parseOptions(
	opts *TestOptions, data string, customOptionParsers map[string]func(string) (CustomOption, bool),
) error {
	hooks := &pebble.ParseHooks{
		NewCache:        pebble.NewCache,
		NewFilterPolicy: filterPolicyFromName,
		SkipUnknown: func(name, value string) bool {
			switch name {
			case "TestOptions":
				return true
			case "TestOptions.strictfs":
				opts.strictFS = true
				opts.Opts.FS = vfs.NewStrictMem()
				return true
			case "TestOptions.ingest_using_apply":
				opts.ingestUsingApply = true
				return true
			case "TestOptions.delete_sized":
				opts.deleteSized = true
				return true
			case "TestOptions.replace_single_delete":
				opts.replaceSingleDelete = true
				return true
			case "TestOptions.use_disk":
				opts.useDisk = true
				opts.Opts.FS = vfs.Default
				return true
			case "TestOptions.initial_state_desc":
				opts.initialStateDesc = value
				return true
			case "TestOptions.initial_state_path":
				opts.initialStatePath = value
				return true
			case "TestOptions.threads":
				v, err := strconv.Atoi(value)
				if err != nil {
					panic(err)
				}
				opts.Threads = v
				return true
			case "TestOptions.disable_block_property_collector":
				v, err := strconv.ParseBool(value)
				if err != nil {
					panic(err)
				}
				opts.disableBlockPropertyCollector = v
				if v {
					opts.Opts.BlockPropertyCollectors = nil
				}
				return true
			case "TestOptions.enable_value_blocks":
				opts.enableValueBlocks = true
				opts.Opts.Experimental.EnableValueBlocks = func() bool { return true }
				return true
			case "TestOptions.disable_value_blocks_for_ingest_sstables":
				opts.disableValueBlocksForIngestSSTables = true
				return true
			case "TestOptions.async_apply_to_db":
				opts.asyncApplyToDB = true
				return true
			case "TestOptions.shared_storage_enabled":
				opts.sharedStorageEnabled = true
				opts.sharedStorageFS = remote.NewInMem()
				if opts.Opts.Experimental.CreateOnShared == remote.CreateOnSharedNone {
					opts.Opts.Experimental.CreateOnShared = remote.CreateOnSharedAll
				}
				return true
			case "TestOptions.external_storage_enabled":
				opts.externalStorageEnabled = true
				opts.externalStorageFS = remote.NewInMem()
				return true
			case "TestOptions.secondary_cache_enabled":
				opts.secondaryCacheEnabled = true
				opts.Opts.Experimental.SecondaryCacheSizeBytes = 1024 * 1024 * 32 // 32 MBs
				return true
			case "TestOptions.seed_efos":
				v, err := strconv.ParseUint(value, 10, 64)
				if err != nil {
					panic(err)
				}
				opts.seedEFOS = v
				return true
			case "TestOptions.io_latency_mean":
				v, err := time.ParseDuration(value)
				if err != nil {
					panic(err)
				}
				opts.ioLatencyMean = v
				return true
			case "TestOptions.io_latency_probability":
				v, err := strconv.ParseFloat(value, 64)
				if err != nil {
					panic(err)
				}
				opts.ioLatencyProbability = v
				return true
			case "TestOptions.io_latency_seed":
				v, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					panic(err)
				}
				opts.ioLatencySeed = v
				return true
			case "TestOptions.ingest_split":
				opts.ingestSplit = true
				opts.Opts.Experimental.IngestSplit = func() bool {
					return true
				}
				return true
			case "TestOptions.use_shared_replicate":
				opts.useSharedReplicate = true
				return true
			case "TestOptions.use_excise":
				opts.useExcise = true
				return true
			default:
				if customOptionParsers == nil {
					return false
				}
				name = strings.TrimPrefix(name, "TestOptions.")
				if p, ok := customOptionParsers[name]; ok {
					if customOpt, ok := p(value); ok {
						opts.CustomOpts = append(opts.CustomOpts, customOpt)
						return true
					}
				}
				return false
			}
		},
	}
	err := opts.Opts.Parse(data, hooks)
	// Ensure that the WAL failover FS agrees with the primary FS. They're
	// separate options, but in the metamorphic tests we keep them in sync.
	if opts.Opts.WALFailover != nil {
		opts.Opts.WALFailover.Secondary.FS = opts.Opts.FS
	}
	opts.InitRemoteStorageFactory()
	opts.Opts.EnsureDefaults()
	return err
}

func optionsToString(opts *TestOptions) string {
	var buf bytes.Buffer
	if opts.strictFS {
		fmt.Fprint(&buf, "  strictfs=true\n")
	}
	if opts.ingestUsingApply {
		fmt.Fprint(&buf, "  ingest_using_apply=true\n")
	}
	if opts.deleteSized {
		fmt.Fprint(&buf, "  delete_sized=true\n")
	}
	if opts.replaceSingleDelete {
		fmt.Fprint(&buf, "  replace_single_delete=true\n")
	}
	if opts.useDisk {
		fmt.Fprint(&buf, "  use_disk=true\n")
	}
	if opts.initialStatePath != "" {
		fmt.Fprintf(&buf, "  initial_state_path=%s\n", opts.initialStatePath)
	}
	if opts.initialStateDesc != "" {
		fmt.Fprintf(&buf, "  initial_state_desc=%s\n", opts.initialStateDesc)
	}
	if opts.Threads != 0 {
		fmt.Fprintf(&buf, "  threads=%d\n", opts.Threads)
	}
	if opts.disableBlockPropertyCollector {
		fmt.Fprintf(&buf, "  disable_block_property_collector=%t\n", opts.disableBlockPropertyCollector)
	}
	if opts.enableValueBlocks {
		fmt.Fprintf(&buf, "  enable_value_blocks=%t\n", opts.enableValueBlocks)
	}
	if opts.disableValueBlocksForIngestSSTables {
		fmt.Fprintf(&buf, "  disable_value_blocks_for_ingest_sstables=%t\n", opts.disableValueBlocksForIngestSSTables)
	}
	if opts.asyncApplyToDB {
		fmt.Fprint(&buf, "  async_apply_to_db=true\n")
	}
	if opts.sharedStorageEnabled {
		fmt.Fprint(&buf, "  shared_storage_enabled=true\n")
	}
	if opts.externalStorageEnabled {
		fmt.Fprint(&buf, "  external_storage_enabled=true\n")
	}
	if opts.secondaryCacheEnabled {
		fmt.Fprint(&buf, "  secondary_cache_enabled=true\n")
	}
	if opts.seedEFOS != 0 {
		fmt.Fprintf(&buf, "  seed_efos=%d\n", opts.seedEFOS)
	}
	if opts.ingestSplit {
		fmt.Fprintf(&buf, "  ingest_split=%v\n", opts.ingestSplit)
	}
	if opts.ioLatencyProbability > 0.0 {
		fmt.Fprintf(&buf, "  io_latency_mean=%s\n", opts.ioLatencyMean)
		fmt.Fprintf(&buf, "  io_latency_probability=%f\n", opts.ioLatencyProbability)
		fmt.Fprintf(&buf, "  io_latency_seed=%d\n", opts.ioLatencySeed)
	}
	if opts.useSharedReplicate {
		fmt.Fprintf(&buf, "  use_shared_replicate=%v\n", opts.useSharedReplicate)
	}
	if opts.useExcise {
		fmt.Fprintf(&buf, "  use_excise=%v\n", opts.useExcise)
	}
	for _, customOpt := range opts.CustomOpts {
		fmt.Fprintf(&buf, "  %s=%s\n", customOpt.Name(), customOpt.Value())
	}

	s := opts.Opts.String()
	if buf.Len() == 0 {
		return s
	}
	return s + "\n[TestOptions]\n" + buf.String()
}

func defaultTestOptions() *TestOptions {
	return &TestOptions{
		Opts:        defaultOptions(),
		Threads:     16,
		RetryPolicy: NeverRetry,
	}
}

func defaultOptions() *pebble.Options {
	opts := &pebble.Options{
		// Use an archive cleaner to ease post-mortem debugging.
		Cleaner: base.ArchiveCleaner{},
		// Always use our custom comparer which provides a Split method,
		// splitting keys at the trailing '@'.
		Comparer:           testkeys.Comparer,
		DebugCheck:         pebble.DebugCheckLevels,
		FS:                 vfs.NewMem(),
		FormatMajorVersion: defaultFormatMajorVersion,
		Levels: []pebble.LevelOptions{{
			FilterPolicy: bloom.FilterPolicy(10),
		}},
		BlockPropertyCollectors: blockPropertyCollectorConstructors,
	}
	return opts
}

// TestOptions describes the options configuring an individual run of the
// metamorphic tests.
type TestOptions struct {
	// Opts holds the *pebble.Options for the test.
	Opts *pebble.Options
	// Threads configures the parallelism of the test. Each thread will run in
	// an independent goroutine and be responsible for executing operations
	// against an independent set of objects. The outcome of any individual
	// operation will still be deterministic, with the metamorphic test
	// inserting synchronization where necessary.
	Threads int
	// RetryPolicy configures which errors should be retried.
	RetryPolicy RetryPolicy
	// CustomOptions holds custom test options that are defined outside of this
	// package.
	CustomOpts []CustomOption

	// internal

	useDisk  bool
	strictFS bool
	// Use Batch.Apply rather than DB.Ingest.
	ingestUsingApply bool
	// Use Batch.DeleteSized rather than Batch.Delete.
	deleteSized bool
	// Replace a SINGLEDEL with a DELETE.
	replaceSingleDelete bool
	// The path on the local filesystem where the initial state of the database
	// exists.  Empty if the test run begins from an empty database state.
	initialStatePath string
	// A human-readable string describing the initial state of the database.
	// Empty if the test run begins from an empty database state.
	initialStateDesc string
	// Disable the block property collector, which may be used by block property
	// filters.
	disableBlockPropertyCollector bool
	// Enable the use of value blocks.
	enableValueBlocks bool
	// Disables value blocks in the sstables written for ingest.
	disableValueBlocksForIngestSSTables bool
	// Use DB.ApplyNoSyncWait for applies that want to sync the WAL.
	asyncApplyToDB bool
	// Enable the use of shared storage.
	sharedStorageEnabled bool
	sharedStorageFS      remote.Storage
	// Enable the use of shared storage for external file ingestion.
	externalStorageEnabled bool
	externalStorageFS      remote.Storage
	// Enables the use of shared replication in TestOptions.
	useSharedReplicate bool
	// Enable the secondary cache. Only effective if sharedStorageEnabled is
	// also true.
	secondaryCacheEnabled bool
	// If nonzero, enables the use of EventuallyFileOnlySnapshots for
	// newSnapshotOps that are keyspan-bounded. The set of which newSnapshotOps
	// are actually created as EventuallyFileOnlySnapshots is deterministically
	// derived from the seed and the operation index.
	seedEFOS uint64
	// If nonzero, enables the injection of random IO latency. The mechanics of
	// a Pebble operation can be very timing dependent, so artificial latency
	// can ensure a wide variety of mechanics are exercised. Additionally,
	// exercising some mechanics such as WAL failover require IO latency.
	ioLatencyProbability float64
	ioLatencySeed        int64
	ioLatencyMean        time.Duration
	// Enables ingest splits. Saved here for serialization as Options does not
	// serialize this.
	ingestSplit bool
	// Enables operations that do excises. Note that a false value for this does
	// not guarantee the lack of excises, as useSharedReplicate can also cause
	// excises. However !useExcise && !useSharedReplicate can be used to guarantee
	// lack of excises.
	useExcise bool
}

// InitRemoteStorageFactory initializes Opts.Experimental.RemoteStorage.
func (testOpts *TestOptions) InitRemoteStorageFactory() {
	if testOpts.sharedStorageEnabled || testOpts.externalStorageEnabled {
		m := make(map[remote.Locator]remote.Storage)
		if testOpts.sharedStorageEnabled {
			m[""] = testOpts.sharedStorageFS
		}
		if testOpts.externalStorageEnabled {
			m["external"] = testOpts.externalStorageFS
		}
		testOpts.Opts.Experimental.RemoteStorage = remote.MakeSimpleFactory(m)
	}
}

// CustomOption defines a custom option that configures the behavior of an
// individual test run. Like all test options, custom options are serialized to
// the OPTIONS file even if they're not options ordinarily understood by Pebble.
type CustomOption interface {
	// Name returns the name of the custom option. This is the key under which
	// the option appears in the OPTIONS file, within the [TestOptions] stanza.
	Name() string
	// Value returns the value of the custom option, serialized as it should
	// appear within the OPTIONS file.
	Value() string
	// Close is run after the test database has been closed at the end of the
	// test as well as during restart operations within the test sequence. It's
	// passed a copy of the *pebble.Options. If the custom options hold on to
	// any resources outside, Close should release them.
	Close(*pebble.Options) error
	// Open is run before the test runs and during a restart operation after the
	// test database has been closed and Close has been called. It's passed a
	// copy of the *pebble.Options. If the custom options must acquire any
	// resources before the test continues, it should reacquire them.
	Open(*pebble.Options) error

	// TODO(jackson): provide additional hooks for custom options changing the
	// behavior of a run.
}

func standardOptions() []*TestOptions {
	// The index labels are not strictly necessary, but they make it easier to
	// find which options correspond to a failure.
	stdOpts := []string{
		0: "", // default options
		1: `
[Options]
  cache_size=1
`,
		2: `
[Options]
  disable_wal=true
`,
		3: `
[Options]
  l0_compaction_threshold=1
`,
		4: `
[Options]
  l0_compaction_threshold=1
  l0_stop_writes_threshold=1
`,
		5: `
[Options]
  lbase_max_bytes=1
`,
		6: `
[Options]
  max_manifest_file_size=1
`,
		7: `
[Options]
  max_open_files=1
`,
		8: `
[Options]
  mem_table_size=2000
`,
		9: `
[Options]
  mem_table_stop_writes_threshold=2
`,
		10: `
[Options]
  wal_dir=data/wal
`,
		11: `
[Level "0"]
  block_restart_interval=1
`,
		12: `
[Level "0"]
  block_size=1
`,
		13: `
[Level "0"]
  compression=NoCompression
`,
		14: `
[Level "0"]
  index_block_size=1
`,
		15: `
[Level "0"]
  target_file_size=1
`,
		16: `
[Level "0"]
  filter_policy=none
`,
		// 1GB
		17: `
[Options]
  bytes_per_sync=1073741824
[TestOptions]
  strictfs=true
`,
		18: `
[Options]
  max_concurrent_compactions=2
`,
		19: `
[TestOptions]
  ingest_using_apply=true
`,
		20: `
[TestOptions]
  replace_single_delete=true
`,
		21: `
[TestOptions]
 use_disk=true
`,
		22: `
[Options]
  max_writer_concurrency=2
  force_writer_parallelism=true
`,
		23: `
[TestOptions]
  disable_block_property_collector=true
`,
		24: `
[TestOptions]
  threads=1
`,
		25: `
[TestOptions]
  enable_value_blocks=true
`,
		26: fmt.Sprintf(`
[Options]
  format_major_version=%s
`, newestFormatMajorVersionToTest),
		27: fmt.Sprintf(`
[Options]
  format_major_version=%s
[TestOptions]
  shared_storage_enabled=true
  secondary_cache_enabled=true
`, pebble.FormatMinForSharedObjects),
		28: fmt.Sprintf(`
[Options]
  format_major_version=%s
[TestOptions]
  external_storage_enabled=true
`, pebble.FormatSyntheticPrefixSuffix),
		29: fmt.Sprintf(`
[Options]
  format_major_version=%s
[TestOptions]
  shared_storage_enabled=true
  external_storage_enabled=true
  secondary_cache_enabled=false
`, pebble.FormatSyntheticPrefixSuffix),
	}

	opts := make([]*TestOptions, len(stdOpts))
	for i := range opts {
		opts[i] = defaultTestOptions()
		// NB: The standard options by definition can never include custom
		// options, so no need to propagate custom option parsers.
		if err := parseOptions(opts[i], stdOpts[i], nil /* custom option parsers */); err != nil {
			panic(err)
		}
	}
	return opts
}

// RandomOptions generates a random set of operations, drawing randomness from
// rng.
func RandomOptions(
	rng *rand.Rand, customOptionParsers map[string]func(string) (CustomOption, bool),
) *TestOptions {
	testOpts := defaultTestOptions()
	opts := testOpts.Opts

	// There are some private options, which we don't want users to fiddle with.
	// There's no way to set it through the public interface. The only method is
	// through Parse.
	{
		var privateOpts bytes.Buffer
		fmt.Fprintln(&privateOpts, `[Options]`)
		if rng.Intn(3) == 0 /* 33% */ {
			fmt.Fprintln(&privateOpts, `  disable_delete_only_compactions=true`)
		}
		if rng.Intn(3) == 0 /* 33% */ {
			fmt.Fprintln(&privateOpts, `  disable_elision_only_compactions=true`)
		}
		if rng.Intn(5) == 0 /* 20% */ {
			fmt.Fprintln(&privateOpts, `  disable_lazy_combined_iteration=true`)
		}
		if privateOptsStr := privateOpts.String(); privateOptsStr != `[Options]\n` {
			parseOptions(testOpts, privateOptsStr, customOptionParsers)
		}
	}

	opts.BytesPerSync = 1 << uint(rng.Intn(28))     // 1B - 256MB
	opts.Cache = cache.New(1 << uint(rng.Intn(30))) // 1B - 1GB
	opts.DisableWAL = rng.Intn(2) == 0
	opts.FlushDelayDeleteRange = time.Millisecond * time.Duration(5*rng.Intn(245)) // 5-250ms
	opts.FlushDelayRangeKey = time.Millisecond * time.Duration(5*rng.Intn(245))    // 5-250ms
	opts.FlushSplitBytes = 1 << rng.Intn(20)                                       // 1B - 1MB
	opts.FormatMajorVersion = minimumFormatMajorVersion
	n := int(newestFormatMajorVersionToTest - opts.FormatMajorVersion)
	opts.FormatMajorVersion += pebble.FormatMajorVersion(rng.Intn(n + 1))
	opts.Experimental.L0CompactionConcurrency = 1 + rng.Intn(4) // 1-4
	opts.Experimental.LevelMultiplier = 5 << rng.Intn(7)        // 5 - 320
	opts.TargetByteDeletionRate = 1 << uint(20+rng.Intn(10))    // 1MB - 1GB
	opts.Experimental.ValidateOnIngest = rng.Intn(2) != 0
	opts.L0CompactionThreshold = 1 + rng.Intn(100)     // 1 - 100
	opts.L0CompactionFileThreshold = 1 << rng.Intn(11) // 1 - 1024
	opts.L0StopWritesThreshold = 1 + rng.Intn(100)     // 1 - 100
	if opts.L0StopWritesThreshold < opts.L0CompactionThreshold {
		opts.L0StopWritesThreshold = opts.L0CompactionThreshold
	}
	opts.LBaseMaxBytes = 1 << uint(rng.Intn(30)) // 1B - 1GB
	maxConcurrentCompactions := rng.Intn(3) + 1  // 1-3
	opts.MaxConcurrentCompactions = func() int {
		return maxConcurrentCompactions
	}
	opts.MaxManifestFileSize = 1 << uint(rng.Intn(30)) // 1B  - 1GB
	opts.MemTableSize = 2 << (10 + uint(rng.Intn(16))) // 2KB - 256MB
	opts.MemTableStopWritesThreshold = 2 + rng.Intn(5) // 2 - 5
	if rng.Intn(2) == 0 {
		opts.WALDir = "data/wal"
	}

	// Half the time enable WAL failover.
	if rng.Intn(2) == 0 {
		// Use 10x longer durations when writing directly to FS; we don't want
		// WAL failover to trigger excessively frequently.
		referenceDur := time.Millisecond
		if testOpts.useDisk {
			referenceDur *= 10
		}

		scaleDuration := func(d time.Duration, minFactor, maxFactor float64) time.Duration {
			return time.Duration(float64(d) * (minFactor + rng.Float64()*(maxFactor-minFactor)))
		}
		unhealthyThreshold := expRandDuration(rng, 3*referenceDur, time.Second)
		healthyThreshold := expRandDuration(rng, 3*referenceDur, time.Second)
		healthyInterval := scaleDuration(healthyThreshold, 1.0, 10.0) // Between 1-10x the healthy threshold
		opts.WALFailover = &pebble.WALFailoverOptions{
			Secondary: wal.Dir{FS: vfs.Default, Dirname: "data/wal_secondary"},
			FailoverOptions: wal.FailoverOptions{
				PrimaryDirProbeInterval:      scaleDuration(healthyThreshold, 0.10, 0.50), // Between 10-50% of the healthy threshold
				HealthyProbeLatencyThreshold: healthyThreshold,
				HealthyInterval:              healthyInterval,
				UnhealthySamplingInterval:    scaleDuration(unhealthyThreshold, 0.10, 0.50), // Between 10-50% of the unhealthy threshold
				UnhealthyOperationLatencyThreshold: func() (time.Duration, bool) {
					return unhealthyThreshold, true
				},
				ElevatedWriteStallThresholdLag: expRandDuration(rng, 5*referenceDur, 2*time.Second),
			},
		}
	}
	if rng.Intn(4) == 0 {
		// Enable Writer parallelism for 25% of the random options. Setting
		// MaxWriterConcurrency to any value greater than or equal to 1 has the
		// same effect currently.
		opts.Experimental.MaxWriterConcurrency = 2
		opts.Experimental.ForceWriterParallelism = true
	}
	if rng.Intn(2) == 0 {
		opts.Experimental.DisableIngestAsFlushable = func() bool { return true }
	}

	// We either use no multilevel compactions, multilevel compactions with the
	// default (zero) additional propensity, or multilevel compactions with an
	// additional propensity to encourage more multilevel compactions than we
	// ohterwise would.
	switch rng.Intn(3) {
	case 0:
		opts.Experimental.MultiLevelCompactionHeuristic = pebble.NoMultiLevel{}
	case 1:
		opts.Experimental.MultiLevelCompactionHeuristic = pebble.WriteAmpHeuristic{}
	default:
		opts.Experimental.MultiLevelCompactionHeuristic = pebble.WriteAmpHeuristic{
			AddPropensity: rng.Float64() * float64(rng.Intn(3)), // [0,3.0)
			AllowL0:       rng.Intn(4) == 1,                     // 25% of the time
		}
	}

	var lopts pebble.LevelOptions
	lopts.BlockRestartInterval = 1 + rng.Intn(64)  // 1 - 64
	lopts.BlockSize = 1 << uint(rng.Intn(24))      // 1 - 16MB
	lopts.BlockSizeThreshold = 50 + rng.Intn(50)   // 50 - 100
	lopts.IndexBlockSize = 1 << uint(rng.Intn(24)) // 1 - 16MB
	lopts.TargetFileSize = 1 << uint(rng.Intn(28)) // 1 - 256MB

	// We either use no bloom filter, the default filter, or a filter with
	// randomized bits-per-key setting. We zero out the Filters map. It'll get
	// repopulated on EnsureDefaults accordingly.
	opts.Filters = nil
	switch rng.Intn(3) {
	case 0:
		lopts.FilterPolicy = nil
	case 1:
		lopts.FilterPolicy = bloom.FilterPolicy(10)
	default:
		lopts.FilterPolicy = newTestingFilterPolicy(1 << rng.Intn(5))
	}

	// We use either no compression, snappy compression or zstd compression.
	switch rng.Intn(3) {
	case 0:
		lopts.Compression = pebble.NoCompression
	case 1:
		lopts.Compression = pebble.ZstdCompression
	default:
		lopts.Compression = pebble.SnappyCompression
	}
	opts.Levels = []pebble.LevelOptions{lopts}

	// Explicitly disable disk-backed FS's for the random configurations. The
	// single standard test configuration that uses a disk-backed FS is
	// sufficient.
	testOpts.useDisk = false
	testOpts.strictFS = rng.Intn(2) != 0 // Only relevant for MemFS.
	// 50% of the time, enable IO latency injection.
	if rng.Intn(2) == 0 {
		testOpts.ioLatencyMean = expRandDuration(rng, 3*time.Millisecond, time.Second)
		testOpts.ioLatencyProbability = 0.01 * rng.Float64() // 0-1%
		testOpts.ioLatencySeed = rng.Int63()
	}
	testOpts.Threads = rng.Intn(runtime.GOMAXPROCS(0)) + 1
	if testOpts.strictFS {
		opts.DisableWAL = false
		opts.FS = vfs.NewStrictMem()
	} else if !testOpts.useDisk {
		opts.FS = vfs.NewMem()
	}
	// Update the WALFailover's secondary to use the same FS. This isn't
	// strictly necessary (the WALFailover could use a separate FS), but it
	// ensures when we save a copy of the test state to disk, we include the
	// secondary's WALs.
	if opts.WALFailover != nil {
		opts.WALFailover.Secondary.FS = opts.FS
	}
	testOpts.ingestUsingApply = rng.Intn(2) != 0
	testOpts.deleteSized = rng.Intn(2) != 0
	testOpts.replaceSingleDelete = rng.Intn(2) != 0
	testOpts.disableBlockPropertyCollector = rng.Intn(2) == 1
	if testOpts.disableBlockPropertyCollector {
		testOpts.Opts.BlockPropertyCollectors = nil
	}
	testOpts.enableValueBlocks = rng.Intn(2) != 0
	if testOpts.enableValueBlocks {
		testOpts.Opts.Experimental.EnableValueBlocks = func() bool { return true }
	}
	testOpts.disableValueBlocksForIngestSSTables = rng.Intn(2) == 0
	testOpts.asyncApplyToDB = rng.Intn(2) != 0
	// 20% of time, enable shared storage.
	if rng.Intn(5) == 0 {
		testOpts.sharedStorageEnabled = true
		if testOpts.Opts.FormatMajorVersion < pebble.FormatMinForSharedObjects {
			testOpts.Opts.FormatMajorVersion = pebble.FormatMinForSharedObjects
		}
		testOpts.sharedStorageFS = remote.NewInMem()
		// If shared storage is enabled, pick between writing all files on shared
		// vs. lower levels only, 50% of the time.
		testOpts.Opts.Experimental.CreateOnShared = remote.CreateOnSharedAll
		if rng.Intn(2) == 0 {
			testOpts.Opts.Experimental.CreateOnShared = remote.CreateOnSharedLower
		}
		// If shared storage is enabled, enable secondary cache 50% of time.
		if rng.Intn(2) == 0 {
			testOpts.secondaryCacheEnabled = true
			// TODO(josh): Randomize various secondary cache settings.
			testOpts.Opts.Experimental.SecondaryCacheSizeBytes = 1024 * 1024 * 32 // 32 MBs
		}
		// 50% of the time, enable shared replication.
		testOpts.useSharedReplicate = rng.Intn(2) == 0
	}

	// 50% of time, enable external storage.
	if rng.Intn(2) == 0 {
		testOpts.externalStorageEnabled = true
		if testOpts.Opts.FormatMajorVersion < pebble.FormatSyntheticPrefixSuffix {
			testOpts.Opts.FormatMajorVersion = pebble.FormatSyntheticPrefixSuffix
		}
		testOpts.externalStorageFS = remote.NewInMem()
	}

	testOpts.seedEFOS = rng.Uint64()
	// TODO(bilal): Enable ingestSplit when known bugs with virtual sstables
	// are addressed.
	//
	// testOpts.ingestSplit = rng.Intn(2) == 0
	opts.Experimental.IngestSplit = func() bool { return testOpts.ingestSplit }
	testOpts.useExcise = rng.Intn(2) == 0
	if testOpts.useExcise {
		if testOpts.Opts.FormatMajorVersion < pebble.FormatVirtualSSTables {
			testOpts.Opts.FormatMajorVersion = pebble.FormatVirtualSSTables
		}
	}
	testOpts.InitRemoteStorageFactory()
	testOpts.Opts.EnsureDefaults()
	return testOpts
}

func expRandDuration(rng *rand.Rand, meanDur, maxDur time.Duration) time.Duration {
	return min(maxDur, time.Duration(math.Round(rng.ExpFloat64()*float64(meanDur))))
}

func setupInitialState(dataDir string, testOpts *TestOptions) error {
	// Copy (vfs.Default,<initialStatePath>/data) to (testOpts.opts.FS,<dataDir>).
	ok, err := vfs.Clone(
		vfs.Default,
		testOpts.Opts.FS,
		vfs.Default.PathJoin(testOpts.initialStatePath, "data"),
		dataDir,
		vfs.CloneSync,
		vfs.CloneSkip(func(filename string) bool {
			// Skip the archive of historical files, any checkpoints created by
			// operations and files staged for ingest in tmp.
			b := filepath.Base(filename)
			return b == "archive" || b == "checkpoints" || b == "tmp"
		}))
	if err != nil {
		return err
	} else if !ok {
		return os.ErrNotExist
	}

	// Tests with wal_dir set store their WALs in a `wal` directory. The source
	// database (initialStatePath) could've had wal_dir set, or the current test
	// options (testOpts) could have wal_dir set, or both.
	//
	// If the test opts are not configured to use a WAL dir, we add the WAL dir
	// as a 'WAL recovery dir' so that we'll read any WALs in the directory in
	// Open.
	walRecoveryPath := testOpts.Opts.FS.PathJoin(dataDir, "wal")
	if testOpts.Opts.WALDir != "" {
		// If the test opts are configured to use a WAL dir, we add the data
		// directory itself as a 'WAL recovery dir' so that we'll read any WALs if
		// the previous test was writing them to the data directory.
		walRecoveryPath = dataDir
	}
	testOpts.Opts.WALRecoveryDirs = append(testOpts.Opts.WALRecoveryDirs, wal.Dir{
		FS:      testOpts.Opts.FS,
		Dirname: walRecoveryPath,
	})

	// If the failover dir exists and the test opts are not configured to use
	// WAL failover, add the failover directory as a 'WAL recovery dir' in case
	// the previous test was configured to use failover.
	failoverDir := testOpts.Opts.FS.PathJoin(dataDir, "wal_secondary")
	if _, err := testOpts.Opts.FS.Stat(failoverDir); err == nil && testOpts.Opts.WALFailover == nil {
		testOpts.Opts.WALRecoveryDirs = append(testOpts.Opts.WALRecoveryDirs, wal.Dir{
			FS:      testOpts.Opts.FS,
			Dirname: failoverDir,
		})
	}
	return nil
}

var blockPropertyCollectorConstructors = []func() pebble.BlockPropertyCollector{
	sstable.NewTestKeysBlockPropertyCollector,
}

// testingFilterPolicy is used to allow bloom filter policies with non-default
// bits-per-key setting. It is necessary because the name of the production
// filter policy is fixed (see bloom.FilterPolicy.Name()); we need to output a
// custom policy name to the OPTIONS file that the test can then parse.
type testingFilterPolicy struct {
	bloom.FilterPolicy
}

var _ pebble.FilterPolicy = (*testingFilterPolicy)(nil)

func newTestingFilterPolicy(bitsPerKey int) *testingFilterPolicy {
	return &testingFilterPolicy{
		FilterPolicy: bloom.FilterPolicy(bitsPerKey),
	}
}

const testingFilterPolicyFmt = "testing_bloom_filter/bits_per_key=%d"

// Name implements the pebble.FilterPolicy interface.
func (t *testingFilterPolicy) Name() string {
	if t.FilterPolicy == 10 {
		return "rocksdb.BuiltinBloomFilter"
	}
	return fmt.Sprintf(testingFilterPolicyFmt, t.FilterPolicy)
}

func filterPolicyFromName(name string) (pebble.FilterPolicy, error) {
	switch name {
	case "none":
		return nil, nil
	case "rocksdb.BuiltinBloomFilter":
		return bloom.FilterPolicy(10), nil
	}
	var bitsPerKey int
	if _, err := fmt.Sscanf(name, testingFilterPolicyFmt, &bitsPerKey); err != nil {
		return nil, errors.Errorf("Invalid filter policy name '%s'", name)
	}
	return newTestingFilterPolicy(bitsPerKey), nil
}
