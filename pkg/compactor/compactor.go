package compactor

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/grafana/loki/v3/pkg/analytics"
	"github.com/grafana/loki/v3/pkg/compactor/deletion"
	"github.com/grafana/loki/v3/pkg/compactor/retention"
	"github.com/grafana/loki/v3/pkg/storage/chunk/client"
	"github.com/grafana/loki/v3/pkg/storage/chunk/client/local"
	chunk_util "github.com/grafana/loki/v3/pkg/storage/chunk/client/util"
	"github.com/grafana/loki/v3/pkg/storage/config"
	"github.com/grafana/loki/v3/pkg/storage/stores/shipper/indexshipper/storage"
	"github.com/grafana/loki/v3/pkg/util/filter"
	util_log "github.com/grafana/loki/v3/pkg/util/log"
	lokiring "github.com/grafana/loki/v3/pkg/util/ring"
	"github.com/grafana/loki/v3/pkg/validation"
)

// Here is how the generic compactor works:
// 1. Find the index type from table name using schemaPeriodForTable.
// 2. Find the registered IndexCompactor for the index type.
// 3. Build an instance of TableCompactor using IndexCompactor.NewIndexCompactor, with all the required information to do a compaction.
// 4. Run the compaction using TableCompactor.Compact, which would set the new/updated CompactedIndex for each IndexSet.
// 5. If retention is enabled, run retention on the CompactedIndex using its retention.IndexProcessor implementation.
// 6. Convert the CompactedIndex to a file using the IndexCompactor.ToIndexFile for uploading.
// 7. If we uploaded successfully, delete the old index files.

const (
	// ringAutoForgetUnhealthyPeriods is how many consecutive timeout periods an unhealthy instance
	// in the ring will be automatically removed.
	ringAutoForgetUnhealthyPeriods = 10

	// ringKey is the key under which we store the store gateways ring in the KVStore.
	ringKey = "compactor"

	// ringNameForServer is the name of the ring used by the compactor server.
	ringNameForServer = "compactor"

	// ringKeyOfLeader is a somewhat arbitrary ID to pull from the ring to see who will be elected the leader
	ringKeyOfLeader = 0

	// ringReplicationFactor should be 1 because we only want to pull back one node from the Ring
	ringReplicationFactor = 1

	// ringNumTokens sets our single token in the ring,
	// we only need to insert 1 token to be used for leader election purposes.
	ringNumTokens = 1
)

var (
	retentionEnabledStats = analytics.NewString("compactor_retention_enabled")
	defaultRetentionStats = analytics.NewString("compactor_default_retention")

	errSchemaForTableNotFound = errors.New("schema for table not found")
)

type Config struct {
	WorkingDirectory               string              `yaml:"working_directory"`
	CompactionInterval             time.Duration       `yaml:"compaction_interval"`
	ApplyRetentionInterval         time.Duration       `yaml:"apply_retention_interval"`
	RetentionEnabled               bool                `yaml:"retention_enabled"`
	RetentionDeleteDelay           time.Duration       `yaml:"retention_delete_delay"`
	RetentionDeleteWorkCount       int                 `yaml:"retention_delete_worker_count"`
	RetentionTableTimeout          time.Duration       `yaml:"retention_table_timeout"`
	RetentionBackoffConfig         backoff.Config      `yaml:"retention_backoff_config"`
	DeleteRequestStore             string              `yaml:"delete_request_store"`
	DeleteRequestStoreKeyPrefix    string              `yaml:"delete_request_store_key_prefix"`
	DeleteRequestStoreDBType       string              `yaml:"delete_request_store_db_type"`
	BackupDeleteRequestStoreDBType string              `yaml:"backup_delete_request_store_db_type"`
	DeleteBatchSize                int                 `yaml:"delete_batch_size"`
	DeleteRequestCancelPeriod      time.Duration       `yaml:"delete_request_cancel_period"`
	DeleteMaxInterval              time.Duration       `yaml:"delete_max_interval"`
	MaxCompactionParallelism       int                 `yaml:"max_compaction_parallelism"`
	UploadParallelism              int                 `yaml:"upload_parallelism"`
	CompactorRing                  lokiring.RingConfig `yaml:"compactor_ring,omitempty" doc:"description=The hash ring configuration used by compactors to elect a single instance for running compactions. The CLI flags prefix for this block config is: compactor.ring"`
	RunOnce                        bool                `yaml:"_" doc:"hidden"`
	TablesToCompact                int                 `yaml:"tables_to_compact"`
	SkipLatestNTables              int                 `yaml:"skip_latest_n_tables"`
}

// RegisterFlags registers flags.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.WorkingDirectory, "compactor.working-directory", "/var/loki/compactor", "Directory where files can be downloaded for compaction.")
	f.DurationVar(&cfg.CompactionInterval, "compactor.compaction-interval", 10*time.Minute, "Interval at which to re-run the compaction operation.")
	f.DurationVar(&cfg.ApplyRetentionInterval, "compactor.apply-retention-interval", 0, "Interval at which to apply/enforce retention. 0 means run at same interval as compaction. If non-zero, it should always be a multiple of compaction interval.")
	f.DurationVar(&cfg.RetentionDeleteDelay, "compactor.retention-delete-delay", 2*time.Hour, "Delay after which chunks will be fully deleted during retention.")
	f.BoolVar(&cfg.RetentionEnabled, "compactor.retention-enabled", false, "Activate custom (per-stream,per-tenant) retention.")
	f.IntVar(&cfg.RetentionDeleteWorkCount, "compactor.retention-delete-worker-count", 150, "The total amount of worker to use to delete chunks.")
	f.StringVar(&cfg.DeleteRequestStore, "compactor.delete-request-store", "", "Store used for managing delete requests.")
	f.StringVar(&cfg.DeleteRequestStoreKeyPrefix, "compactor.delete-request-store.key-prefix", "index/", "Path prefix for storing delete requests.")
	f.StringVar(&cfg.DeleteRequestStoreDBType, "compactor.delete-request-store.db-type", string(deletion.DeleteRequestsStoreDBTypeBoltDB), fmt.Sprintf("Type of DB to use for storing delete requests. Supported types: %s", strings.Join(*(*[]string)(unsafe.Pointer(&deletion.SupportedDeleteRequestsStoreDBTypes)), ", ")))
	f.StringVar(&cfg.BackupDeleteRequestStoreDBType, "compactor.delete-request-store.backup-db-type", "", fmt.Sprintf("Type of DB to use as backup for storing delete requests. Backup DB should ideally be used while migrating from one DB type to another. Supported type(s): %s", deletion.DeleteRequestsStoreDBTypeBoltDB))
	f.IntVar(&cfg.DeleteBatchSize, "compactor.delete-batch-size", 70, "The max number of delete requests to run per compaction cycle.")
	f.DurationVar(&cfg.DeleteRequestCancelPeriod, "compactor.delete-request-cancel-period", 24*time.Hour, "Allow cancellation of delete request until duration after they are created. Data would be deleted only after delete requests have been older than this duration. Ideally this should be set to at least 24h.")
	f.DurationVar(&cfg.DeleteMaxInterval, "compactor.delete-max-interval", 24*time.Hour, "Constrain the size of any single delete request with line filters. When a delete request > delete_max_interval is input, the request is sharded into smaller requests of no more than delete_max_interval")
	f.DurationVar(&cfg.RetentionTableTimeout, "compactor.retention-table-timeout", 0, "The maximum amount of time to spend running retention and deletion on any given table in the index.")
	f.IntVar(&cfg.MaxCompactionParallelism, "compactor.max-compaction-parallelism", 1, "Maximum number of tables to compact in parallel. While increasing this value, please make sure compactor has enough disk space allocated to be able to store and compact as many tables.")
	f.IntVar(&cfg.UploadParallelism, "compactor.upload-parallelism", 10, "Number of upload/remove operations to execute in parallel when finalizing a compaction. NOTE: This setting is per compaction operation, which can be executed in parallel. The upper bound on the number of concurrent uploads is upload_parallelism * max_compaction_parallelism.")
	f.BoolVar(&cfg.RunOnce, "compactor.run-once", false, "Run the compactor one time to cleanup and compact index files only (no retention applied)")
	f.IntVar(&cfg.TablesToCompact, "compactor.tables-to-compact", 0, "Number of tables that compactor will try to compact. Newer tables are chosen when this is less than the number of tables available.")
	f.IntVar(&cfg.SkipLatestNTables, "compactor.skip-latest-n-tables", 0, "Do not compact N latest tables. Together with -compactor.run-once and -compactor.tables-to-compact, this is useful when clearing compactor backlogs.")

	cfg.RetentionBackoffConfig.RegisterFlagsWithPrefix("compactor.retention-backoff-config", f)
	// Ring
	skipFlags := []string{
		"compactor.ring.num-tokens",
		"compactor.ring.replication-factor",
	}
	cfg.CompactorRing.RegisterFlagsWithPrefix("compactor.", "collectors/", f, skipFlags...)
	f.IntVar(&cfg.CompactorRing.NumTokens, "compactor.ring.num-tokens", ringNumTokens, fmt.Sprintf("IGNORED: Num tokens is fixed to %d", ringNumTokens))
	f.IntVar(&cfg.CompactorRing.ReplicationFactor, "compactor.ring.replication-factor", ringReplicationFactor, fmt.Sprintf("IGNORED: Replication factor is fixed to %d", ringReplicationFactor))
}

// Validate verifies the config does not contain inappropriate values
func (cfg *Config) Validate() error {
	if cfg.MaxCompactionParallelism < 1 {
		return errors.New("max compaction parallelism must be >= 1")
	}

	if cfg.CompactorRing.NumTokens != ringNumTokens {
		return errors.New("Num tokens must not be changed as it will not take effect")
	}

	if cfg.CompactorRing.ReplicationFactor != ringReplicationFactor {
		return errors.New("Replication factor must not be changed as it will not take effect")
	}

	if cfg.RetentionEnabled {
		if cfg.DeleteRequestStore == "" {
			return fmt.Errorf("compactor.delete-request-store should be configured when retention is enabled")
		}

		if cfg.ApplyRetentionInterval == 0 {
			cfg.ApplyRetentionInterval = cfg.CompactionInterval
		}

		if cfg.ApplyRetentionInterval == cfg.CompactionInterval {
			// add some jitter to avoid running retention and compaction at same time
			cfg.ApplyRetentionInterval += min(10*time.Minute, cfg.ApplyRetentionInterval/2)
		}

		if err := config.ValidatePathPrefix(cfg.DeleteRequestStoreKeyPrefix); err != nil {
			return fmt.Errorf("validate delete store path prefix: %w", err)
		}
	}

	return nil
}

type Compactor struct {
	services.Service

	cfg                       Config
	indexStorageClient        storage.Client
	tableMarker               retention.TableMarker
	sweeper                   *retention.Sweeper
	deleteRequestsStore       deletion.DeleteRequestsStore
	DeleteRequestsHandler     *deletion.DeleteRequestHandler
	DeleteRequestsGRPCHandler *deletion.GRPCRequestHandler
	deleteRequestsManager     *deletion.DeleteRequestsManager
	expirationChecker         retention.ExpirationChecker
	metrics                   *metrics
	running                   bool
	wg                        sync.WaitGroup
	indexCompactors           map[string]IndexCompactor
	schemaConfig              config.SchemaConfig
	limits                    Limits

	tablesManager *tablesManager
	// Ring used for running a single compactor
	ringLifecycler *ring.BasicLifecycler
	ring           *ring.Ring
	ringPollPeriod time.Duration

	// Subservices manager.
	subservices        *services.Manager
	subservicesWatcher *services.FailureWatcher

	// one for each period
	storeContainers map[config.DayTime]storeContainer
}

type storeContainer struct {
	tableMarker        retention.TableMarker
	sweeper            *retention.Sweeper
	indexStorageClient storage.Client
}

type Limits interface {
	deletion.Limits
	retention.Limits
	DefaultLimits() *validation.Limits
}

func NewCompactor(
	cfg Config,
	objectStoreClients map[config.DayTime]client.ObjectClient,
	deleteStoreClient client.ObjectClient,
	schemaConfig config.SchemaConfig,
	limits Limits,
	indexUpdatePropagationMaxDelay time.Duration,
	r prometheus.Registerer,
	metricsNamespace string,
) (*Compactor, error) {
	retentionEnabledStats.Set("false")
	if cfg.RetentionEnabled {
		retentionEnabledStats.Set("true")
	}
	if limits != nil {
		defaultRetentionStats.Set(limits.DefaultLimits().RetentionPeriod.String())
	}

	compactor := &Compactor{
		cfg:             cfg,
		ringPollPeriod:  5 * time.Second,
		indexCompactors: map[string]IndexCompactor{},
		schemaConfig:    schemaConfig,
		limits:          limits,
	}

	ringStore, err := kv.NewClient(
		cfg.CompactorRing.KVStore,
		ring.GetCodec(),
		kv.RegistererWithKVName(prometheus.WrapRegistererWithPrefix("loki_", r), "compactor"),
		util_log.Logger,
	)
	if err != nil {
		return nil, errors.Wrap(err, "create KV store client")
	}
	lifecyclerCfg, err := cfg.CompactorRing.ToLifecyclerConfig(ringNumTokens, util_log.Logger)
	if err != nil {
		return nil, errors.Wrap(err, "invalid ring lifecycler config")
	}

	// Define lifecycler delegates in reverse order (last to be called defined first because they're
	// chained via "next delegate").
	delegate := ring.BasicLifecyclerDelegate(compactor)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, util_log.Logger)
	delegate = ring.NewTokensPersistencyDelegate(cfg.CompactorRing.TokensFilePath, ring.JOINING, delegate, util_log.Logger)
	delegate = ring.NewAutoForgetDelegate(ringAutoForgetUnhealthyPeriods*cfg.CompactorRing.HeartbeatTimeout, delegate, util_log.Logger)

	compactor.ringLifecycler, err = ring.NewBasicLifecycler(lifecyclerCfg, ringNameForServer, ringKey, ringStore, delegate, util_log.Logger, r)
	if err != nil {
		return nil, errors.Wrap(err, "create ring lifecycler")
	}

	ringCfg := cfg.CompactorRing.ToRingConfig(ringReplicationFactor)
	compactor.ring, err = ring.NewWithStoreClientAndStrategy(ringCfg, ringNameForServer, ringKey, ringStore, ring.NewIgnoreUnhealthyInstancesReplicationStrategy(), prometheus.WrapRegistererWithPrefix(metricsNamespace+"_", r), util_log.Logger)
	if err != nil {
		return nil, errors.Wrap(err, "create ring client")
	}

	compactor.subservices, err = services.NewManager(compactor.ringLifecycler, compactor.ring)
	if err != nil {
		return nil, err
	}
	compactor.subservicesWatcher = services.NewFailureWatcher()
	compactor.subservicesWatcher.WatchManager(compactor.subservices)

	if err := compactor.init(objectStoreClients, deleteStoreClient, schemaConfig, indexUpdatePropagationMaxDelay, limits, r); err != nil {
		return nil, fmt.Errorf("init compactor: %w", err)
	}

	compactor.Service = services.NewBasicService(compactor.starting, compactor.loop, compactor.stopping)
	return compactor, nil
}

func (c *Compactor) init(
	objectStoreClients map[config.DayTime]client.ObjectClient,
	deleteStoreClient client.ObjectClient,
	schemaConfig config.SchemaConfig,
	indexUpdatePropagationMaxDelay time.Duration,
	limits Limits,
	r prometheus.Registerer,
) error {
	err := chunk_util.EnsureDirectory(c.cfg.WorkingDirectory)
	if err != nil {
		return err
	}

	if c.cfg.RetentionEnabled {
		if deleteStoreClient == nil {
			return fmt.Errorf("delete store client not initialised when retention is enabled")
		}

		if err := c.initDeletes(deleteStoreClient, indexUpdatePropagationMaxDelay, r, limits); err != nil {
			return fmt.Errorf("failed to init delete store: %w", err)
		}
	} else {
		c.expirationChecker = retention.NeverExpiringExpirationChecker(limits)
	}

	legacyMarkerDirs := make(map[string]struct{})
	c.storeContainers = make(map[config.DayTime]storeContainer, len(objectStoreClients))
	for from, objectClient := range objectStoreClients {
		period, err := schemaConfig.SchemaForTime(from.Time)
		if err != nil {
			return err
		}

		var sc storeContainer
		sc.indexStorageClient = storage.NewIndexStorageClient(objectClient, period.IndexTables.PathPrefix)

		if c.cfg.RetentionEnabled {
			var (
				raw              client.ObjectClient
				encoder          client.KeyEncoder
				name             = fmt.Sprintf("%s_%s", period.ObjectType, period.From.String())
				retentionWorkDir = filepath.Join(c.cfg.WorkingDirectory, "retention", name)
				r                = prometheus.WrapRegistererWith(prometheus.Labels{"from": name}, r)
			)

			// given that compaction can now run on multiple periods, marker files are stored under /retention/{objectStoreType}_{periodFrom}/markers/
			// if any markers are found in the common markers dir (/retention/markers/) or store specific markers dir (/retention/{objectStoreType}/markers/), copy them to the period specific dirs
			// chunk would be removed by the sweeper if it belongs to a given period or no-op if it doesn't exist.
			if err := retention.CopyMarkers(filepath.Join(c.cfg.WorkingDirectory, "retention"), retentionWorkDir); err != nil {
				return fmt.Errorf("failed to move common markers to period specific dir: %w", err)
			}

			if err := retention.CopyMarkers(filepath.Join(c.cfg.WorkingDirectory, "retention", period.ObjectType), retentionWorkDir); err != nil {
				return fmt.Errorf("failed to move store markers to period specific dir: %w", err)
			}
			// remove markers from the store dir after copying them to period specific dirs.
			legacyMarkerDirs[period.ObjectType] = struct{}{}

			if casted, ok := objectClient.(client.PrefixedObjectClient); ok {
				raw = casted.GetDownstream()
			} else {
				raw = objectClient
			}
			if _, ok := raw.(*local.FSObjectClient); ok {
				encoder = client.FSEncoder
			}
			chunkClient := client.NewClient(objectClient, encoder, schemaConfig)

			sc.sweeper, err = retention.NewSweeper(retentionWorkDir, chunkClient, c.cfg.RetentionDeleteWorkCount, c.cfg.RetentionDeleteDelay, c.cfg.RetentionBackoffConfig, r)
			if err != nil {
				return fmt.Errorf("failed to init sweeper: %w", err)
			}

			sc.tableMarker, err = retention.NewMarker(retentionWorkDir, c.expirationChecker, c.cfg.RetentionTableTimeout, chunkClient, r)
			if err != nil {
				return fmt.Errorf("failed to init table marker: %w", err)
			}
		}

		c.storeContainers[from] = sc
	}

	if c.cfg.RetentionEnabled {
		// remove legacy markers
		for store := range legacyMarkerDirs {
			if err := os.RemoveAll(filepath.Join(c.cfg.WorkingDirectory, "retention", store, retention.MarkersFolder)); err != nil {
				return fmt.Errorf("remove old markers from store dir: %w", err)
			}
		}

		if err := os.RemoveAll(filepath.Join(c.cfg.WorkingDirectory, "retention", retention.MarkersFolder)); err != nil {
			return fmt.Errorf("remove old markers: %w", err)
		}
	}

	c.metrics = newMetrics(r)
	c.tablesManager = newTablesManager(c.cfg, c.storeContainers, c.indexCompactors, c.schemaConfig, c.expirationChecker, c.metrics)

	if c.cfg.RetentionEnabled {
		if err := c.deleteRequestsManager.Init(c.tablesManager); err != nil {
			return err
		}
	}

	return nil
}

func (c *Compactor) initDeletes(objectClient client.ObjectClient, indexUpdatePropagationMaxDelay time.Duration, r prometheus.Registerer, limits Limits) error {
	deletionWorkDir := filepath.Join(c.cfg.WorkingDirectory, "deletion")
	indexStorageClient := storage.NewIndexStorageClient(objectClient, c.cfg.DeleteRequestStoreKeyPrefix)
	store, err := deletion.NewDeleteRequestsStore(
		deletion.DeleteRequestsStoreDBType(c.cfg.DeleteRequestStoreDBType),
		deletionWorkDir,
		indexStorageClient,
		deletion.DeleteRequestsStoreDBType(c.cfg.BackupDeleteRequestStoreDBType),
		indexUpdatePropagationMaxDelay,
	)
	if err != nil {
		return err
	}

	c.deleteRequestsStore = store

	c.DeleteRequestsHandler = deletion.NewDeleteRequestHandler(
		c.deleteRequestsStore,
		c.cfg.DeleteMaxInterval,
		c.cfg.DeleteRequestCancelPeriod,
		r,
	)

	c.DeleteRequestsGRPCHandler = deletion.NewGRPCRequestHandler(c.deleteRequestsStore, limits)

	c.deleteRequestsManager, err = deletion.NewDeleteRequestsManager(
		deletionWorkDir,
		c.deleteRequestsStore,
		c.cfg.DeleteRequestCancelPeriod,
		c.cfg.DeleteBatchSize,
		limits,
		false,
		objectClient,
		r,
	)
	if err != nil {
		return err
	}

	c.expirationChecker = newExpirationChecker(retention.NewExpirationChecker(limits), c.deleteRequestsManager)
	return nil
}

func (c *Compactor) starting(ctx context.Context) (err error) {
	// In case this function will return error we want to unregister the instance
	// from the ring. We do it ensuring dependencies are gracefully stopped if they
	// were already started.
	defer func() {
		if err == nil || c.subservices == nil {
			return
		}

		if stopErr := services.StopManagerAndAwaitStopped(context.Background(), c.subservices); stopErr != nil {
			level.Error(util_log.Logger).Log("msg", "failed to gracefully stop compactor dependencies", "err", stopErr)
		}
	}()

	if err := services.StartManagerAndAwaitHealthy(ctx, c.subservices); err != nil {
		return errors.Wrap(err, "unable to start compactor subservices")
	}

	// The BasicLifecycler does not automatically move state to ACTIVE such that any additional work that
	// someone wants to do can be done before becoming ACTIVE. For the query compactor we don't currently
	// have any additional work so we can become ACTIVE right away.

	// Wait until the ring client detected this instance in the JOINING state to
	// make sure that when we'll run the initial sync we already know  the tokens
	// assigned to this instance.
	level.Info(util_log.Logger).Log("msg", "waiting until compactor is JOINING in the ring")
	if err := ring.WaitInstanceState(ctx, c.ring, c.ringLifecycler.GetInstanceID(), ring.JOINING); err != nil {
		return err
	}
	level.Info(util_log.Logger).Log("msg", "compactor is JOINING in the ring")

	// Change ring state to ACTIVE
	if err = c.ringLifecycler.ChangeState(ctx, ring.ACTIVE); err != nil {
		return errors.Wrapf(err, "switch instance to %s in the ring", ring.ACTIVE)
	}

	// Wait until the ring client detected this instance in the ACTIVE state to
	// make sure that when we'll run the loop it won't be detected as a ring
	// topology change.
	level.Info(util_log.Logger).Log("msg", "waiting until compactor is ACTIVE in the ring")
	if err := ring.WaitInstanceState(ctx, c.ring, c.ringLifecycler.GetInstanceID(), ring.ACTIVE); err != nil {
		return err
	}
	level.Info(util_log.Logger).Log("msg", "compactor is ACTIVE in the ring")

	return nil
}

func (c *Compactor) loop(ctx context.Context) error {
	if c.cfg.RunOnce {
		level.Info(util_log.Logger).Log("msg", "running single compaction")
		err := c.tablesManager.runCompaction(ctx, false)
		if err != nil {
			level.Error(util_log.Logger).Log("msg", "compaction encountered an error", "err", err)
		}
		level.Info(util_log.Logger).Log("msg", "single compaction finished")
		level.Info(util_log.Logger).Log("msg", "interrupt or terminate the process to finish")

		// Wait for Loki to shutdown.
		<-ctx.Done()
		level.Info(util_log.Logger).Log("msg", "compactor exiting")
		return nil
	}

	if c.cfg.RetentionEnabled {
		if c.deleteRequestsStore != nil {
			defer c.deleteRequestsStore.Stop()
		}
	}

	syncTicker := time.NewTicker(c.ringPollPeriod)
	defer syncTicker.Stop()

	var runningCtx context.Context
	var runningCancel context.CancelFunc
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			if runningCancel != nil {
				runningCancel()
			}
			wg.Wait()
			level.Info(util_log.Logger).Log("msg", "compactor exiting")
			return nil
		case <-syncTicker.C:
			bufDescs, bufHosts, bufZones := ring.MakeBuffersForGet()
			rs, err := c.ring.Get(ringKeyOfLeader, ring.Write, bufDescs, bufHosts, bufZones)
			if err != nil {
				level.Error(util_log.Logger).Log("msg", "error asking ring for who should run the compactor, will check again", "err", err)
				continue
			}

			addrs := rs.GetAddresses()
			if len(addrs) != 1 {
				level.Error(util_log.Logger).Log("msg", "too many addresses (more that one) return when asking the ring who should run the compactor, will check again")
				continue
			}
			if c.ringLifecycler.GetInstanceAddr() == addrs[0] {
				// If not running, start
				if !c.running {
					level.Info(util_log.Logger).Log("msg", "this instance has been chosen to run the compactor, starting compactor")
					runningCtx, runningCancel = context.WithCancel(ctx)

					wg.Add(1)
					go func() {
						defer wg.Done()
						c.tablesManager.start(runningCtx)
					}()

					if c.deleteRequestsManager != nil {
						wg.Add(1)
						go func() {
							defer wg.Done()
							c.deleteRequestsManager.Start(runningCtx)
						}()
					}

					c.running = true
					c.metrics.compactorRunning.Set(1)
				}
			} else {
				// If running, shutdown
				if c.running {
					level.Info(util_log.Logger).Log("msg", "this instance should no longer run the compactor, stopping compactor")
					runningCancel()
					wg.Wait()
					c.running = false
					c.metrics.compactorRunning.Set(0)
					level.Info(util_log.Logger).Log("msg", "compactor stopped")
				}
			}
		}
	}
}

func (c *Compactor) stopping(_ error) error {
	return services.StopManagerAndAwaitStopped(context.Background(), c.subservices)
}

func (c *Compactor) RegisterIndexCompactor(indexType string, indexCompactor IndexCompactor) {
	c.indexCompactors[indexType] = indexCompactor
}

func (c *Compactor) TablesManager() TablesManager {
	return c.tablesManager
}

type expirationChecker struct {
	retentionExpiryChecker retention.ExpirationChecker
	deletionExpiryChecker  retention.ExpirationChecker
}

func newExpirationChecker(retentionExpiryChecker, deletionExpiryChecker retention.ExpirationChecker) retention.ExpirationChecker {
	return &expirationChecker{retentionExpiryChecker, deletionExpiryChecker}
}

func (e *expirationChecker) Expired(userID []byte, chk retention.Chunk, lbls labels.Labels, seriesID []byte, tableName string, now model.Time) (bool, filter.Func) {
	if expired, nonDeletedIntervals := e.retentionExpiryChecker.Expired(userID, chk, lbls, seriesID, tableName, now); expired {
		return expired, nonDeletedIntervals
	}

	return e.deletionExpiryChecker.Expired(userID, chk, lbls, seriesID, tableName, now)
}

func (e *expirationChecker) MarkPhaseStarted() {
	e.retentionExpiryChecker.MarkPhaseStarted()
	e.deletionExpiryChecker.MarkPhaseStarted()
}

func (e *expirationChecker) MarkPhaseFailed() {
	e.retentionExpiryChecker.MarkPhaseFailed()
	e.deletionExpiryChecker.MarkPhaseFailed()
}

func (e *expirationChecker) MarkPhaseFinished() {
	e.retentionExpiryChecker.MarkPhaseFinished()
	e.deletionExpiryChecker.MarkPhaseFinished()
}

func (e *expirationChecker) MarkPhaseTimedOut() {
	e.retentionExpiryChecker.MarkPhaseTimedOut()
	e.deletionExpiryChecker.MarkPhaseTimedOut()
}

func (e *expirationChecker) IntervalMayHaveExpiredChunks(interval model.Interval, userID string) bool {
	return e.retentionExpiryChecker.IntervalMayHaveExpiredChunks(interval, userID) || e.deletionExpiryChecker.IntervalMayHaveExpiredChunks(interval, userID)
}

func (e *expirationChecker) DropFromIndex(userID []byte, chk retention.Chunk, labels labels.Labels, tableEndTime model.Time, now model.Time) bool {
	return e.retentionExpiryChecker.DropFromIndex(userID, chk, labels, tableEndTime, now) || e.deletionExpiryChecker.DropFromIndex(userID, chk, labels, tableEndTime, now)
}

func (e *expirationChecker) CanSkipSeries(userID []byte, lbls labels.Labels, seriesID []byte, seriesStart model.Time, tableName string, now model.Time) bool {
	return e.retentionExpiryChecker.CanSkipSeries(userID, lbls, seriesID, seriesStart, tableName, now) && e.deletionExpiryChecker.CanSkipSeries(userID, lbls, seriesID, seriesStart, tableName, now)
}

func (e *expirationChecker) MarkSeriesAsProcessed(userID, seriesID []byte, lbls labels.Labels, tableName string) error {
	if err := e.retentionExpiryChecker.MarkSeriesAsProcessed(userID, seriesID, lbls, tableName); err != nil {
		return err
	}

	return e.deletionExpiryChecker.MarkSeriesAsProcessed(userID, seriesID, lbls, tableName)
}

func (c *Compactor) OnRingInstanceRegister(_ *ring.BasicLifecycler, ringDesc ring.Desc, instanceExists bool, _ string, instanceDesc ring.InstanceDesc) (ring.InstanceState, ring.Tokens) {
	// When we initialize the compactor instance in the ring we want to start from
	// a clean situation, so whatever is the state we set it JOINING, while we keep existing
	// tokens (if any) or the ones loaded from file.
	var tokens []uint32
	if instanceExists {
		tokens = instanceDesc.GetTokens()
	}

	takenTokens := ringDesc.GetTokens()
	gen := ring.NewRandomTokenGenerator()
	newTokens := gen.GenerateTokens(ringNumTokens-len(tokens), takenTokens)

	// Tokens sorting will be enforced by the parent caller.
	tokens = append(tokens, newTokens...)

	return ring.JOINING, tokens
}

func (c *Compactor) OnRingInstanceTokens(_ *ring.BasicLifecycler, _ ring.Tokens) {}
func (c *Compactor) OnRingInstanceStopping(_ *ring.BasicLifecycler)              {}
func (c *Compactor) OnRingInstanceHeartbeat(_ *ring.BasicLifecycler, _ *ring.Desc, _ *ring.InstanceDesc) {
}

func SortTablesByRange(tables []string) {
	tableRanges := make(map[string]model.Interval)
	for _, table := range tables {
		tableRanges[table] = retention.ExtractIntervalFromTableName(table)
	}

	sort.Slice(tables, func(i, j int) bool {
		// less than if start time is after produces a most recent first sort order
		return tableRanges[tables[i]].Start.After(tableRanges[tables[j]].Start)
	})
}

func SchemaPeriodForTable(cfg config.SchemaConfig, tableName string) (config.PeriodConfig, bool) {
	tableInterval := retention.ExtractIntervalFromTableName(tableName)
	schemaCfg, err := cfg.SchemaForTime(tableInterval.Start)
	if err != nil || schemaCfg.IndexTables.TableFor(tableInterval.Start) != tableName {
		return config.PeriodConfig{}, false
	}

	return schemaCfg, true
}
