// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/ruler/ruler_ring.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package ruler

import (
	"flag"
	"net"
	"strconv"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/ring"

	"github.com/grafana/mimir/pkg/util"
)

var (
	// RuleEvalRingOp is the operation used for distributing rule groups between rulers.
	RuleEvalRingOp = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, func(s ring.InstanceState) bool {
		// Only ACTIVE rulers get any rule groups. If instance is not ACTIVE, we need to find another ruler.
		return s != ring.ACTIVE
	})

	RuleSyncRingOp = ring.NewOp([]ring.InstanceState{ring.JOINING, ring.ACTIVE}, func(s ring.InstanceState) bool {
		// Only ACTIVE or JOINING rulers can sync rule groups. If instance is not ACTIVE NOR JOINING, we need to find another ruler.
		return s != ring.ACTIVE && s != ring.JOINING
	})
)

// RingConfig masks the ring lifecycler config which contains
// many options not really required by the rulers ring. This config
// is used to strip down the config to the minimum, and avoid confusion
// to the user.
type RingConfig struct {
	Common util.CommonRingConfig `yaml:",inline"`

	AutoForgetUnhealthyPeriods int `yaml:"auto_forget_unhealthy_periods" category:"advanced"`

	NumTokens int `yaml:"num_tokens" category:"advanced"`

	// Used for testing
	SkipUnregister bool `yaml:"-"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *RingConfig) RegisterFlags(f *flag.FlagSet, logger log.Logger) {
	const flagNamePrefix = "ruler.ring."
	const kvStorePrefix = "rulers/"
	const componentPlural = "rulers"
	cfg.Common.RegisterFlags(flagNamePrefix, kvStorePrefix, componentPlural, f, logger)

	f.IntVar(&cfg.AutoForgetUnhealthyPeriods, flagNamePrefix+"auto-forget-unhealthy-periods", 2, "Number of consecutive timeout periods an unhealthy instance in the ring is automatically removed after. Set to 0 to disable auto-forget.")

	f.IntVar(&cfg.NumTokens, flagNamePrefix+"num-tokens", 128, "Number of tokens for each ruler.")
}

// ToLifecyclerConfig returns a LifecyclerConfig based on the ruler
// ring config.
func (cfg *RingConfig) ToLifecyclerConfig(logger log.Logger) (ring.BasicLifecyclerConfig, error) {
	instanceAddr, err := ring.GetInstanceAddr(cfg.Common.InstanceAddr, cfg.Common.InstanceInterfaceNames, logger, cfg.Common.EnableIPv6)
	if err != nil {
		return ring.BasicLifecyclerConfig{}, err
	}

	instancePort := ring.GetInstancePort(cfg.Common.InstancePort, cfg.Common.ListenPort)

	return ring.BasicLifecyclerConfig{
		ID:                  cfg.Common.InstanceID,
		Addr:                net.JoinHostPort(instanceAddr, strconv.Itoa(instancePort)),
		HeartbeatPeriod:     cfg.Common.HeartbeatPeriod,
		HeartbeatTimeout:    cfg.Common.HeartbeatTimeout,
		TokensObservePeriod: 0,
		NumTokens:           cfg.NumTokens,
	}, nil
}

func (cfg *RingConfig) toRingConfig() ring.Config {
	rc := cfg.Common.ToRingConfig()
	rc.SubringCacheDisabled = true

	// Each rule group is loaded to *exactly* one ruler.
	rc.ReplicationFactor = 1

	return rc
}
