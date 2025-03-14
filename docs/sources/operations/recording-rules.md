---
title: Manage recording rules
menuTitle: Recording rules
description: Describes how to setup and use recording rules in Grafana Loki.
weight:  
---
# Manage recording rules

Recording rules are queries that run in an interval and produce metrics from logs that can be pushed to a Prometheus compatible backend.

Recording rules are evaluated by the `ruler` component. Each `ruler` acts as its own `querier`, in the sense that it
executes queries against the store without using the `query-frontend` or `querier` components. It will respect all query
[limits](https://grafana.com/docs/loki/<LOKI_VERSION>/configure/#limits_config) put in place for the `querier`.

The Loki implementation of recording rules largely reuses Prometheus' code.

Samples generated by recording rules are sent to Prometheus using Prometheus' **remote-write** feature.

## Write-Ahead Log (WAL)

All samples generated by recording rules are written to a WAL. The WALs main benefit is that it persists the samples
generated by recording rules to disk, which means that if your `ruler` crashes, you won't lose any data.
We are trading off extra memory usage and slower start-up times for this functionality.

A WAL is created per tenant; this is done to prevent cross-tenant interactions. If all samples were to be written
to a single WAL, this would increase the chances that one tenant could cause data-loss for others. A typical scenario here
is that Prometheus will, for example, reject a remote-write request with 100 samples if just 1 of those samples is invalid in some way.

### Start-up

When the `ruler` starts up, it will load the WALs for the tenants who have recording rules. These WAL files are stored
on disk and are loaded into memory.

{{< admonition type="note" >}}
WALs are loaded one at a time upon start-up. This is a current limitation of the Loki ruler.
For this reason, it is adviseable that the number of rule groups serviced by a ruler be kept to a reasonable size, since
_no rule evaluation occurs while WAL replay is in progress (this includes alerting rules)_.
{{< /admonition >}}


### Truncation

WAL files are regularly truncated to reduce their size on disk.
[This guide](https://ganeshvernekar.com/blog/prometheus-tsdb-wal-and-checkpoint/#wal-truncation-and-checkpointing)
from one of the Prometheus maintainers (Ganesh Vernekar) gives an excellent overview of the truncation, checkpointing,
and replaying of the WAL.

### Cleaner

<span style="background-color:#f3f973;">WAL Cleaner is an experimental feature.</span>

The WAL Cleaner watches for abandoned WALs (tenants who no longer have recording rules associated) and deletes them.
Enable this feature only if you are running into storage concerns with WALs that are too large. WALs should not grow
excessively large due to truncation.

## Scaling

See Mimir's guide for [configuring Grafana Mimir hash rings](/docs/mimir/latest/configure/configure-hash-rings/) for scaling the ruler using a ring.

{{< admonition type="note" >}}
The `ruler` shards by rule _group_, not by individual rules. This is an artifact of the fact that Prometheus
recording rules need to run in order since one recording rule can reuse another - but this is not possible in Loki.
{{< /admonition >}}

## Deployment

The `ruler` needs to persist its WAL files to disk, and it incurs a bit of a start-up cost by reading these WALs into memory.
As such, it is recommended that you try to minimize churn of individual `ruler` instances since rule evaluation is blocked
while the WALs are being read from disk.

### Kubernetes

It is recommended that you run the `rulers` using `StatefulSets`. The `ruler` will write its WAL files to persistent storage,
so a `Persistent Volume` should be utilised.

## Remote-Write

### Client configuration

Remote-write client configuration is fully compatible with [prometheus configuration format](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#remote_write).

```yaml
remote_write:	
  clients:	
    mimir:	
      url: http://mimir/api/v1/push
      write_relabel_configs:
      - action: replace
        target_label: job
        replacement: loki-recording-rules
```

### Per-Tenant Limits

Remote-write can be configured at a global level in the base configuration, and certain parameters tuned specifically on
a per-tenant basis. Most of the configuration options [defined here](https://grafana.com/docs/loki/<LOKI_VERSION>/configure/#ruler)
have [override options](https://grafana.com/docs/loki/<LOKI_VERSION>/configure/#limits_config) (which can be also applied at runtime!).

### Tuning

Remote-write can be tuned if the default configuration is insufficient (see [Failure Modes](#failure-modes) below).

There is a [guide](https://prometheus.io/docs/practices/remote_write/) on the Prometheus website, all of which applies to Loki, too.

Rules can be evenly distributed across available rulers by using `-ruler.enable-sharding=true` and `-ruler.sharding-strategy="by-rule"`.
Rule groups execute in order; this is a feature inherited from Prometheus' rule engine (which Loki uses), but Loki has no
need for this constraint because rules cannot depend on each other. The default sharding strategy will shard by rule groups,
but this may be undesirable as some rule groups could contain more expensive rules, which can lead to subsequent rules missing evaluations.
The `by-rule` sharding strategy creates one rule group for each rule the ruler instance "owns" (based on its hash ring), and these rings
are all executed concurrently.

## Observability

Since Loki reuses the Prometheus code for recording rules and WALs, it also gains all of Prometheus' observability.

Prometheus exposes a number of metrics for its WAL implementation, and these have all been prefixed with `loki_ruler_wal_`.

For example: `prometheus_remote_storage_bytes_total` → `loki_ruler_wal_prometheus_remote_storage_bytes_total`

Additional metrics are exposed, also with the prefix `loki_ruler_wal_`. All per-tenant metrics contain a `tenant`
label, so be aware that cardinality could begin to be a concern if the number of tenants grows sufficiently large.

Some key metrics to note are:
- `loki_ruler_wal_appender_ready`: whether a WAL appender is ready to accept samples (1) or not (0)
- `loki_ruler_wal_prometheus_remote_storage_samples_total`: number of samples sent per tenant to remote storage
- `loki_ruler_wal_prometheus_remote_storage_samples...`
  - `loki_ruler_wal_prometheus_remote_storage_samples_pending_total`: samples buffered in memory, waiting to be sent to remote storage
  - `loki_ruler_wal_prometheus_remote_storage_samples_failed_total`: samples that failed when sent to remote storage
  - `loki_ruler_wal_prometheus_remote_storage_samples_dropped_total`: samples dropped by relabel configurations
  - `loki_ruler_wal_prometheus_remote_storage_samples_retried_total`: samples re-resent to remote storage
- `loki_ruler_wal_prometheus_remote_storage_highest_timestamp_in_seconds`: highest timestamp of sample appended to WAL
- `loki_ruler_wal_prometheus_remote_storage_queue_highest_sent_timestamp_seconds`: highest timestamp of sample sent to remote storage.

We've created a basic [dashboard in our loki-mixin](https://github.com/grafana/loki/tree/main/production/loki-mixin/dashboards/recording-rules.libsonnet)
which you can use to administer recording rules.

## Failure Modes

### Remote-Write Lagging

Remote-write can lag behind for many reasons:

1. Remote-write storage (Prometheus) is temporarily unavailable
2. A tenant is producing samples too quickly from a recording rule
3. Remote-write is tuned too low, creating backpressure

It can be determined by subtracting
`loki_ruler_wal_prometheus_remote_storage_queue_highest_sent_timestamp_seconds` from
`loki_ruler_wal_prometheus_remote_storage_highest_timestamp_in_seconds`.

In case 1, the `ruler` will continue to retry sending these samples until the remote storage becomes available again. Be
aware that if the remote storage is down for longer than `ruler.wal.max-age`, data loss may occur after truncation occurs.

In cases 2 and 3, you should consider [tuning](#tuning) remote-write appropriately.

Further reading: see [this blog post](/blog/2021/04/12/how-to-troubleshoot-remote-write-issues-in-prometheus/)
by Prometheus maintainer Callum Styan.

### Appender Not Ready

Each tenant's WAL has an "appender" internally; this appender is used to _append_ samples to the WAL. The appender is marked
as _not ready_ until the WAL replay is complete upon startup. If the WAL is corrupted for some reason, or is taking a long
time to replay, you can determine this by alerting on `loki_ruler_wal_appender_ready < 1`.

### Corrupt WAL

If a disk fails or the `ruler` does not terminate correctly, there's a chance one or more tenant WALs can become corrupted.
A mechanism exists for automatically repairing the WAL, but this cannot handle every conceivable scenario. In this case,
the `loki_ruler_wal_corruptions_repair_failed_total` metric will be incremented.

### Found another failure mode?

Open an [issue](https://github.com/grafana/loki/issues) and tell us about it!

