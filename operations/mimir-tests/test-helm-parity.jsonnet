local k = import 'ksonnet-util/kausal.libsonnet';
local mimir = import 'mimir/mimir.libsonnet';

mimir {
  _config+:: {
    namespace: 'default',
    external_url: 'mimir.default.svc.cluster.local',
    storage_backend: 'gcs',

    blocks_storage_bucket_name: 'example-blocks-bucket',
    alertmanager_storage_bucket_name: 'example-alertmanager-bucket',
    ruler_storage_bucket_name: 'example-ruler-bucket',
    alertmanager_enabled: true,
    ruler_enabled: true,
    unregister_ingesters_on_shutdown: false,
    query_sharding_enabled: true,
    overrides_exporter_enabled: true,

    alertmanager+: {
      fallback_config: {
        route: { receiver: 'default-receiver' },
        receivers: [{ name: 'default-receiver' }],
      },
    },
  },

  // The store-gateway should request the same memory as Helm so that GOMEMLIMIT
  // gets set to the same value.
  store_gateway_container+::
    k.util.resourcesRequestsMixin(null, '512Mi'),

  // These are properties that are set differently on different components in jsonnet.
  // We unset them all here so the default values are used like in Helm.
  // At that point there will likely be less deviation between components.
  // See the tracking issue: https://github.com/grafana/mimir/issues/2749
  querier_args+:: {
    'querier.max-partial-query-length': null,
  },

  query_frontend_args+:: {
    'server.grpc-max-recv-msg-size-bytes': null,
    'query-frontend.query-sharding-total-shards': null,
  },

  ruler_args+:: {
    'querier.max-partial-query-length': null,
  },
}
