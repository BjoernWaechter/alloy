# Upstream Compatibility Cliffs Ledger

A running record of upstream API breaks encountered while updating Alloy's key
dependencies. The point of this document is search: when a future upgrader hits
a compile error like "undefined: parser.ParseMetric" or "not enough arguments
in call to scrape.NewManager", they should be able to grep this file by the
symbol name and find the migration pattern that fixed it.

## What is a "cliff"?

A cliff is an upstream API break that requires non-trivial Alloy-side changes
to absorb. The break can take many shapes:

- A function gains or loses arguments
- A free function moves to become a method on an interface
- A struct field changes type, gets renamed, or is moved into a nested struct
- A whole feature flag/option gets removed because the feature went stable
- Two cooperating upstreams (e.g. Prometheus and otel-contrib) ship versions
  that no longer line up — the cliff is then "no single pair of released
  versions works together"

Each entry below names the version transition, the symptom you would see in a
build log, the migration pattern, and (when known) an upstream reference.
Snippets are intentionally short — they exist to confirm "yes, this is the
break you are looking at" rather than to be copy-pasted verbatim.

## How to use this doc

1. Run the build and capture the failing symbol or struct field.
2. Search this file by that exact symbol.
3. Cross-reference the upstream commit/PR linked under "Reference" to confirm
   that the break you are seeing is the one documented here. If upstream has
   since reverted or evolved further, this doc may be stale — update it.
4. Apply the migration pattern.
5. If you discover a new cliff during your upgrade, add an entry here before
   merging. Future-you will thank you.

## How to add a new entry

- Keep entries scoped to a single API break. If one upstream PR breaks five
  call sites in Alloy, that is still one entry — list the call sites under
  "Affected files".
- Prefer concrete `before` / `after` snippets over prose. A 6-line diff is
  worth a page of explanation.
- Always include the symptom string a future upgrader would paste into a
  search engine.
- Link the upstream commit or PR when known. If only the rough date is known,
  record that — it lets the next person bisect.

---

## Prometheus (`github.com/prometheus/prometheus`)

### v0.310 → v0.311: `scrape.NewManager` gained `AppendableV2`

- **Symptom**: `not enough arguments in call to scrape.NewManager`
- **Migration**: The signature went from 5 args to 6. The new arg is an
  `AppendableV2` that slots in just before the existing appendable. Pass `nil`
  if you do not have a v2 appendable.

  ```go
  // Before
  mgr := scrape.NewManager(opts, logger, newScrapePool, appendable, sdMetrics)

  // After
  mgr := scrape.NewManager(opts, logger, newScrapePool, nil, appendable, sdMetrics)
  ```

- **Affected files**: `internal/component/prometheus/scrape/scrape.go`

### v0.310 → v0.311: `discovery.CreateAndRegisterSDMetrics` return type flip

- **Symptom**: None at the Alloy call site — this entry exists specifically to
  reassure future upgraders that they have not missed a migration.
- **Detail**: The return type went from `map[string]DiscovererMetrics` to
  `*SDMetrics`, and `discovery.NewManager` accepts the new type. Because Alloy
  uses `:=` and lets Go infer the variable type, the value still flows
  end-to-end without any source change. If you see a stale build cache
  complaining about the old type, a `go clean -cache` is enough.

### v0.310 → v0.311: `rulefmt.Parse` gained `*slog.Logger`

- **Symptom**: `not enough arguments in call to rulefmt.Parse`
- **Migration**: Append a 5th argument — either `nil` or a real logger.

  ```go
  // Before
  groups, errs := rulefmt.Parse(content, ignoreUnknownFields, scheme, group)

  // After
  groups, errs := rulefmt.Parse(content, ignoreUnknownFields, scheme, group, nil)
  ```

- **Affected files**:
  - `internal/component/loki/rules/kubernetes/events.go`
  - `internal/component/common/kubernetes/prometheus_rule_group_diff_test.go`

### v0.310 → v0.311: PromQL parser free functions removed

- **Symptom**:
  - `undefined: parser.ParseExpr`
  - `undefined: parser.ParseMetric`
  - `undefined: parser.ParseMetricSelector`
- **Detail**: The package-level helper functions were removed. The same
  behaviour now lives on the `Parser` interface returned by `parser.NewParser`.
- **Migration**: Construct a parser and call the method.

  ```go
  // Before
  m, err := parser.ParseMetric(s)

  // After
  m, err := parser.NewParser(parser.Options{}).ParseMetric(s)
  ```

  The same shape applies to `ParseExpr` and `ParseMetricSelector`. If you call
  the parser many times in a hot path, hoist the `NewParser` call out of the
  loop — it is non-trivial to construct.

- **Affected files**:
  - `internal/static/agentctl/waltools/samples.go`
  - `internal/mimir/client/types.go`
  - `internal/component/mimir/rules/kubernetes/events.go`
  - `internal/component/loki/source/api/routes.go`
  - `internal/pipelinetest/harness/sink.go`

### v0.310 → v0.311: `relabel.Process` removed in favour of `ProcessBuilder`

- **Symptom**: `undefined: relabel.Process`
- **Detail**: The old function allocated a fresh `Labels` per call. The new
  API takes a `labels.Builder` so callers can amortise allocations.
- **Migration**: Wrap the input labels in a builder, call `ProcessBuilder`,
  and read the result back out.

  ```go
  // Before
  out, keep := relabel.Process(lbls, cfgs...)

  // After
  builder := labels.NewBuilder(lbls)
  keep := relabel.ProcessBuilder(builder, cfgs...)
  out := builder.Labels()
  ```

- **Affected files**: This pattern appears in many call sites across
  `internal/component/prometheus/...`, `internal/component/discovery/...`,
  and the static-mode shims. A grep for `relabel.Process(` is the fastest way
  to find them all.

### v0.310 → v0.311: alertmanager URL type moved to `prometheus/common`

- **Symptom**: `undefined: config.URL` referencing
  `github.com/prometheus/alertmanager/config`
- **Migration**: Import `github.com/prometheus/common/config` (typically
  aliased `commoncfg`) and use `commoncfg.URL`. The type is structurally
  identical.
- **Affected files**: `internal/mimir/alertmanager/types.go`

### v0.310 → v0.311 (post Apr 24 2026): `parser.Options.ExperimentalDurationExpr` removed

- **Symptom**: `unknown field ExperimentalDurationExpr in struct literal of
  type parser.Options`
- **Detail**: Upstream commit `1463a5bb5a6f` ("PromQL: Promote duration
  expressions as stable", 2026-04-24) removed the experimental gate because the
  feature is now always-on. The field reference survives in `prom-label-proxy
  v0.13.0`, which is why this break can surface even if no Alloy code touches
  the field directly.
- **Migration**: Remove the field reference. For downstream forks that you
  cannot fix in-place, apply a one-line patch that no-ops the setter (see
  `prom-label-proxy v0.13.0` below).
- **Reference**: `github.com/prometheus/prometheus` commit `1463a5bb5a6f`.

### v0.311.x: `azuread.OAuthConfig.ClientSecret` type oscillation

- **Symptom**: `cannot use string(c.ClientSecret) (...) as common.Secret` or
  the inverse.
- **Detail**: The field type bounced between `string` and `config.Secret`
  across patch releases. Released `v0.311.3` and recent `main` snapshots have
  `config.Secret`.
- **Migration**: Cast through `common.Secret` rather than `string`:

  ```go
  // Pin against current v0.311.3 / main
  oauth.ClientSecret = common.Secret(c.ClientSecret)
  ```

- **Affected files**: `internal/component/prometheus/remotewrite/types.go`

### v0.311 cooperating-version cliff

- **Symptom**: No combination of released Prometheus `v0.311.x` and released
  otel-contrib (`v0.147` … `v0.151`) compiles together as-is.
- **Detail**:
  - otel-contrib `v0.147`–`v0.149` was built against released Prometheus
    `v0.310` and calls `scrape.NewManager` with 5 args + the old SDMetrics
    return type.
  - otel-contrib `v0.150`+ was built against a `main` snapshot of Prometheus
    en route to `v3.12` and calls `scrape.NewManager` with 6 args + the new
    `*SDMetrics`.
  - Released `v0.311.x` Prometheus matches neither half cleanly — it has the
    new `scrape.NewManager` signature but lacks some of the other `main`-only
    changes otel-contrib `v0.150`+ relies on.
- **Resolution**: Pin Prometheus to a `main`-branch snapshot (pseudo-version,
  via `dependency-replacements.yaml`) that includes everything both halves
  need. See "Snapshot-pin pattern" in `key-dep-updates.md`. This stays the
  recommended approach until Prometheus `v3.12.0` GA, at which point a
  released pair should exist.

---

## OpenTelemetry Collector Contrib (`github.com/open-telemetry/opentelemetry-collector-contrib`)

### v0.150 → v0.151: `splunkhecexporter.DeprecatedBatchConfig` removed

- **Symptom**:
  - `undefined: splunkhecexporter.DeprecatedBatchConfig`
  - `unknown field DeprecatedBatcher in struct literal of type splunkhecexporter.Config`
- **Detail**: The wrapper configs were marked deprecated in `v0.150` and
  removed in `v0.151`. Functionality moved 1:1 into the exporter helper's
  `sending_queue.batch` block.
- **Migration**: Drop the wrapper. Map old fields onto `sending_queue.batch`:
  - `min_size_items` → `sending_queue.batch.min_size`
  - `max_size_items` → `sending_queue.batch.max_size`
  - `flush_timeout` → `sending_queue.batch.flush_timeout`
- **Affected files**: `internal/component/otelcol/exporter/splunkhec/...`

### v0.150 → v0.151: `otlpreceiver.Config` moved `GRPC`/`HTTP` under `Protocols`

- **Symptom**: `cfg.GRPC undefined (type otlpreceiver.Config has no field or
  method GRPC, but does have field Protocols)`
- **Migration**: Path through the new nesting.

  ```go
  // Before
  cfg.GRPC.NetAddr = ...
  cfg.HTTP.Endpoint = ...

  // After
  cfg.Protocols.GRPC.NetAddr = ...
  cfg.Protocols.HTTP.Endpoint = ...
  ```

- **Affected files**: `internal/component/otelcol/receiver/otlp/...`

### v0.150 → v0.151: `bearertokenauthextension.Authenticate` canonicalises headers

- **Symptom**: A unit test that passes a non-canonical header key
  (e.g. `"authorization"` rather than `"Authorization"`) starts failing
  authentication.
- **Detail**: The extension now does its primary lookup with
  `http.CanonicalHeaderKey(header)`, falling back to lowercase only as a
  compatibility shim. Production code that builds headers via
  `http.Header.Set` is unaffected; tests that hand-craft a `map[string][]string`
  may not be.
- **Migration**: Canonicalise the key in the test fixture.

  ```go
  // Before
  hdr := map[string][]string{"authorization": {token}}

  // After
  hdr := map[string][]string{http.CanonicalHeaderKey("authorization"): {token}}
  ```

### v0.150 → v0.151: `servicegraphconnector` Type renamed `servicegraph` → `service_graph`

- **Symptom**: Test YAML fixtures fail to unmarshal because the connector ID
  no longer matches.
- **Detail**: The old name is preserved as `DeprecatedType` for backward
  compatibility with config files, but new code (and fresh fixtures) should
  use `service_graph`.
- **Migration**: Update test YAML fixtures from `servicegraph:` to
  `service_graph:`. Do NOT rename the Alloy component — the Alloy registry
  name `otelcol.connector.servicegraph` is part of Alloy's public API.

### v0.150 → v0.151: `configmiddleware.Config` is a new wrapper struct

- **Symptom**: When unwrapping Alloy's HTTP/GRPC config wrappers, the
  upstream `confighttp.ClientConfig` / `ServerConfig` and
  `configgrpc.ClientConfig` / `ServerConfig` structs gained a
  `Middlewares []configmiddleware.Config` field that Alloy does not populate.
- **Detail**: This is technically not a break — the zero value is fine — but
  exposing the field to Alloy users requires shipping an
  `otelcol.extension.middleware.*` component family that produces a
  `configmiddleware.Config`. Document the field as "not yet exposed" in the
  changelog rather than silently leaving it off the schema.
- **Affected wrappers**: `HTTPClientArguments`, `HTTPServerArguments`,
  `GRPCClientArguments`, `GRPCServerArguments`.

### v0.150 → v0.151: new fields on `confighttp.ServerConfig`

- **Symptom**: User-facing arguments fall behind upstream defaults.
- **Detail**: `ServerConfig` gained `ResponseHeaders`, `ReadTimeout`,
  `ReadHeaderTimeout`, `WriteTimeout`, and `IdleTimeout`. Every Alloy HTTP
  receiver inherits these.
- **Action**: Expose the new fields on `HTTPServerArguments`. High user value
  — these are the standard Go HTTP timeouts plus a response-headers map.

### v0.150 → v0.151: new `UserAgent` on `configgrpc.ClientConfig`

- **Detail**: `ClientConfig` gained `UserAgent string`. Not exposed by Alloy's
  `GRPCClientArguments` — add it during this transition.

### v0.150 → v0.151: new `ReloadClientCAFile` on `configtls.ServerConfig`

- **Detail**: `ServerConfig` gained `ReloadClientCAFile bool`. Not exposed by
  Alloy's `TLSServerArguments` — add it during this transition.

---

## prometheus-operator (`github.com/prometheus-operator/prometheus-operator`)

### v0.86 → v0.91: `Scheme` fields became typed pointers

- **Symptom**: `cannot use ep.Scheme (variable of type *monitoringv1.Scheme)
  as string` or the inverse.
- **Detail**: `Endpoint.Scheme`, `PodMetricsEndpoint.Scheme`, and
  `ProberSpec.Scheme` all changed from `string` to `*Scheme` (a typed string
  alias, pointer for "unset").
- **Migration**: Nil-check before dereferencing; cast when assigning.

  ```go
  // Before
  if ep.Scheme != "" {
      cfg.Scheme = ep.Scheme
  }

  // After
  if ep.Scheme != nil && *ep.Scheme != "" {
      cfg.Scheme = string(*ep.Scheme)
  }
  ```

- **Affected files**:
  - `internal/component/prometheus/operator/configgen/config_gen_podmonitor.go`
  - `internal/component/prometheus/operator/configgen/config_gen_probe.go`
  - `internal/component/prometheus/operator/configgen/config_gen_scrapeconfig.go`
  - `internal/component/prometheus/operator/configgen/config_gen_servicemonitor.go`

### v0.86 → v0.91: `Endpoint.EnableHttp2` renamed `EnableHTTP2`

- **Symptom**: `ep.EnableHttp2 undefined`
- **Detail**: `PodMetricsEndpoint` already used the canonical
  `EnableHTTP2` spelling — `Endpoint` now matches.
- **Migration**: Pure rename.
- **Affected files**: `config_gen_servicemonitor.go`

### v0.86 → v0.91: `HTTPSDConfig.URL` is now a typed string alias

- **Symptom**: `cannot use cfg.URL (variable of type URL) as string`
- **Migration**: Explicit cast.

  ```go
  // Before
  out.URL = cfg.URL

  // After
  out.URL = string(cfg.URL)
  ```

- **Affected files**: `config_gen_scrapeconfig.go`

### v0.86 → v0.91: `Probe.Spec.BearerTokenSecret` became a pointer

- **Symptom**: `cannot use probe.Spec.BearerTokenSecret (variable of type
  *v1.SecretKeySelector) as v1.SecretKeySelector`
- **Detail**: The field was changed to `*v1.SecretKeySelector` and was
  simultaneously marked deprecated upstream — the recommended replacement is
  the `Authorization` field.
- **Migration**: Add a nil check; tag the deprecated read with
  `//nolint:staticcheck`.

  ```go
  //nolint:staticcheck // BearerTokenSecret is deprecated upstream; use Authorization.
  if probe.Spec.BearerTokenSecret != nil {
      cfg.BearerTokenSecret = *probe.Spec.BearerTokenSecret
  }
  ```

### v0.86 → v0.91: `HTTPConfig` hierarchy reshuffled

- **Symptom**: Test struct literals stop compiling with errors like
  `unknown field EnableHTTP2 in struct literal of type HTTPConfig`, even
  though `cfg.EnableHTTP2` still works at read/write time.
- **Detail**: `HTTPConfig`, `HTTPConfigWithProxy`, and
  `HTTPConfigWithProxyAndTLSFiles` now embed several inner structs. Field
  access still works via Go's promotion rules, but **struct literal
  initialisation does not see promoted fields** — literals must spell out the
  nesting.
- **Migration**: In tests, switch to explicit nested literals.

  ```go
  // Before
  HTTPConfig{
      EnableHTTP2:     true,
      FollowRedirects: ptr(true),
      TLSConfig:       &TLSConfig{ /* ... */ },
      ProxyConfig:     ProxyConfig{ /* ... */ },
  }

  // After
  HTTPConfig{
      ProxyConfig: ProxyConfig{ /* ... */ },
      InnerHTTPConfig: InnerHTTPConfig{
          EnableHTTP2:     true,
          FollowRedirects: ptr(true),
          TLSConfig:       &TLSConfig{ /* ... */ },
      },
  }
  ```

  (Inner struct names vary; consult the upstream definition for the exact
  embed name in the version you are pinning.)

### v0.86 → v0.91: `TLSConfig` embeds `TLSFilesConfig`

- **Detail**: `CAFile`, `KeyFile`, and `CertFile` moved out of `TLSConfig`
  proper and into an embedded `TLSFilesConfig`. Same Go-promotion-vs-literals
  story as the `HTTPConfig` entry above — tests that initialise these fields
  inline need to spell out the nesting.

### prometheus-operator v0.91.0 against a Prometheus `main` snapshot

- **Symptom**: `prometheus-operator/pkg/operator/rules.go:234` fails to
  compile with "not enough arguments in call to rulefmt.Parse".
- **Detail**: `v0.91.0` pins released Prometheus `v0.311.3`, which still has
  the 4-arg `rulefmt.Parse`. When Alloy pins Prometheus to a `main` snapshot
  that already has the 5-arg signature, prom-operator's vendored call site
  breaks.
- **Resolution**: One-line fork patch that adds the trailing `nil` to the
  internal call. Track upstream until prom-operator publishes a release built
  against the new signature, then drop the fork.

---

## prom-label-proxy (`github.com/prometheus-community/prom-label-proxy`)

### v0.13.0 against a Prometheus `main` snapshot (post Apr 24 2026)

- **Symptom**: `unknown field ExperimentalDurationExpr in struct literal of
  type parser.Options`
- **Detail**: `prom-label-proxy v0.13.0` calls a `WithPromqlDurationExpressionParsing`
  setter that flips `parser.Options.ExperimentalDurationExpr` — a field that
  Prometheus removed when it promoted duration expressions to stable (commit
  `1463a5bb5a6f`, 2026-04-24).
- **Resolution**: One-line fork patch that turns the setter into a no-op (the
  feature is unconditionally enabled upstream now). Drop the fork once
  prom-label-proxy publishes a release built against post-Apr-24 Prometheus.
