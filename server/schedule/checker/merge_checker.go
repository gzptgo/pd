// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package checker

import (
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/pd/pkg/cache"
	"github.com/pingcap/pd/server/core"
	"github.com/pingcap/pd/server/namespace"
	"github.com/pingcap/pd/server/schedule/operator"
	"github.com/pingcap/pd/server/schedule/opt"
	"go.uber.org/zap"
)

// MergeChecker ensures region to merge with adjacent region when size is small
type MergeChecker struct {
	cluster    opt.Cluster
	classifier namespace.Classifier
	splitCache *cache.TTLUint64
	startTime  time.Time // it's used to judge whether server recently start.
}

// NewMergeChecker creates a merge checker.
func NewMergeChecker(cluster opt.Cluster, classifier namespace.Classifier) *MergeChecker {
	splitCache := cache.NewIDTTL(time.Minute, cluster.GetSplitMergeInterval())
	return &MergeChecker{
		cluster:    cluster,
		classifier: classifier,
		splitCache: splitCache,
		startTime:  time.Now(),
	}
}

// RecordRegionSplit put the recently split region into cache. MergeChecker
// will skip check it for a while.
func (m *MergeChecker) RecordRegionSplit(regionIDs []uint64) {
	for _, regionID := range regionIDs {
		m.splitCache.PutWithTTL(regionID, nil, m.cluster.GetSplitMergeInterval())
	}
}

// Check verifies a region's replicas, creating an Operator if need.
func (m *MergeChecker) Check(region *core.RegionInfo) []*operator.Operator {
	expireTime := m.startTime.Add(m.cluster.GetSplitMergeInterval())
	if time.Now().Before(expireTime) {
		checkerCounter.WithLabelValues("merge_checker", "recently-start").Inc()
		return nil
	}

	if m.splitCache.Exists(region.GetID()) {
		checkerCounter.WithLabelValues("merge_checker", "recently-split").Inc()
		return nil
	}

	checkerCounter.WithLabelValues("merge_checker", "check").Inc()

	// when pd just started, it will load region meta from etcd
	// but the size for these loaded region info is 0
	// pd don't know the real size of one region until the first heartbeat of the region
	// thus here when size is 0, just skip.
	if region.GetApproximateSize() == 0 {
		checkerCounter.WithLabelValues("merge_checker", "skip").Inc()
		return nil
	}

	// region is not small enough
	if region.GetApproximateSize() > int64(m.cluster.GetMaxMergeRegionSize()) ||
		region.GetApproximateKeys() > int64(m.cluster.GetMaxMergeRegionKeys()) {
		checkerCounter.WithLabelValues("merge_checker", "no-need").Inc()
		return nil
	}

	// skip region has down peers or pending peers or learner peers
	if len(region.GetDownPeers()) > 0 || len(region.GetPendingPeers()) > 0 || len(region.GetLearners()) > 0 {
		checkerCounter.WithLabelValues("merge_checker", "special-peer").Inc()
		return nil
	}

	if len(region.GetPeers()) != m.cluster.GetMaxReplicas() {
		checkerCounter.WithLabelValues("merge_checker", "abnormal-replica").Inc()
		return nil
	}

	// skip hot region
	if m.cluster.IsRegionHot(region) {
		checkerCounter.WithLabelValues("merge_checker", "hot-region").Inc()
		return nil
	}

	prev, next := m.cluster.GetAdjacentRegions(region)

	var target *core.RegionInfo
	if m.checkTarget(region, next) {
		target = next
	}
	if !m.cluster.IsOneWayMergeEnabled() && m.checkTarget(region, prev) { // allow a region can be merged by two ways.
		if target == nil || prev.GetApproximateSize() < next.GetApproximateSize() { // pick smaller
			target = prev
		}
	}

	if target == nil {
		checkerCounter.WithLabelValues("merge_checker", "no-target").Inc()
		return nil
	}

	log.Debug("try to merge region", zap.Stringer("from", core.RegionToHexMeta(region.GetMeta())), zap.Stringer("to", core.RegionToHexMeta(target.GetMeta())))
	ops, err := operator.CreateMergeRegionOperator("merge-region", m.cluster, region, target, operator.OpMerge)
	if err != nil {
		log.Warn("create merge region operator failed", zap.Error(err))
		return nil
	}
	checkerCounter.WithLabelValues("merge_checker", "new-operator").Inc()
	if region.GetApproximateSize() > target.GetApproximateSize() ||
		region.GetApproximateKeys() > target.GetApproximateKeys() {
		checkerCounter.WithLabelValues("merge_checker", "larger-source").Inc()
	}
	return ops
}

func (m *MergeChecker) checkTarget(region, adjacent *core.RegionInfo) bool {
	return adjacent != nil && !m.cluster.IsRegionHot(adjacent) &&
		m.classifier.AllowMerge(region, adjacent) &&
		len(adjacent.GetDownPeers()) == 0 && len(adjacent.GetPendingPeers()) == 0 && len(adjacent.GetLearners()) == 0 && // no special peer
		len(adjacent.GetPeers()) == m.cluster.GetMaxReplicas() // peer count should equal
}
