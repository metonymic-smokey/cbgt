// @author Couchbase <info@couchbase.com>
// @copyright 2016-Present Couchbase, Inc.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
// in that file, in accordance with the Business Source License, use of this
// software will be governed by the Apache License, Version 2.0, included in
// the file licenses/APL2.txt.

package ctl

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/couchbase/cbgt"
	"github.com/couchbase/cbgt/hibernate"
	"github.com/couchbase/cbgt/rebalance"
	"github.com/couchbase/cbgt/rest"

	"github.com/couchbase/cbauth/service"
	log "github.com/couchbase/clog"
)

// Timeout for CtlMgr's exported APIs
var CtlMgrTimeout = time.Duration(20 * time.Second)

// CtlMgr implements the cbauth/service.Manager interface and
// provides the adapter or glue between ns-server's service API
// and cbgt's Ctl implementation.
type CtlMgr struct {
	nodeInfo *service.NodeInfo

	ctl *Ctl

	taskProgressCh chan taskProgress

	mu sync.Mutex // Protects the fields that follow.

	revNumNext uint64 // The next rev num to use.

	tasks       tasks
	tasksWaitCh chan struct{} // Closed when the tasks change.

	lastTaskList service.TaskList

	lastTopologyM sync.Mutex
	lastTopology  service.Topology
}

type tasks struct {
	revNum      uint64
	taskHandles []*taskHandle
}

type taskHandle struct { // The taskHandle fields are immutable.
	startTime time.Time
	task      *service.Task
	stop      func() // May be nil.
}

type taskProgress struct {
	taskId         string
	errs           []error
	progressExists bool
	progress       float64
}

// ------------------------------------------------

func NewCtlMgr(nodeInfo *service.NodeInfo, ctl *Ctl) *CtlMgr {
	m := &CtlMgr{
		nodeInfo:       nodeInfo,
		ctl:            ctl,
		revNumNext:     1,
		tasks:          tasks{revNum: 0},
		taskProgressCh: make(chan taskProgress, 10),
	}

	go func() {
		for taskProgress := range m.taskProgressCh {
			m.handleTaskProgress(taskProgress)
		}
	}()

	return m
}

func (m *CtlMgr) GetNodeInfo() (*service.NodeInfo, error) {
	log.Printf("ctl/manager: GetNodeInfo")

	return m.nodeInfo, nil
}

func (m *CtlMgr) Shutdown() error {
	log.Printf("ctl/manager: Shutdown")

	os.Exit(0)
	return nil
}

func (m *CtlMgr) GetTaskList(haveTasksRev service.Revision,
	cancelCh service.Cancel) (*service.TaskList, error) {
	m.mu.Lock()

	if len(haveTasksRev) > 0 {
		haveTasksRevNum, err := DecodeRev(haveTasksRev)
		if err != nil {
			m.mu.Unlock()

			log.Errorf("ctl/manager: GetTaskList, DecodeRev"+
				", haveTasksRev: %s, err: %v", haveTasksRev, err)

			return nil, err
		}

	OUTER:
		for haveTasksRevNum == m.tasks.revNum {
			if m.tasksWaitCh == nil {
				m.tasksWaitCh = make(chan struct{})
			}
			tasksWaitCh := m.tasksWaitCh

			m.mu.Unlock()
			select {
			case <-cancelCh:
				return nil, service.ErrCanceled

			case <-time.After(CtlMgrTimeout):
				// TIMEOUT
				m.mu.Lock()
				break OUTER

			case <-tasksWaitCh:
				// FALLTHRU
			}
			m.mu.Lock()
		}
	}

	rv := m.getTaskListLOCKED()

	m.lastTaskList.Rev = rv.Rev
	same := reflect.DeepEqual(&m.lastTaskList, rv)
	m.lastTaskList = *rv

	m.mu.Unlock()

	if !same {
		log.Printf("ctl/manager: GetTaskList, haveTasksRev: %s,"+
			" changed, rv: %+v", haveTasksRev, rv)
	}

	return rv, nil
}

func (m *CtlMgr) CancelTask(
	taskId string, taskRev service.Revision) error {
	log.Printf("ctl/manager: CancelTask, taskId: %s, taskRev: %s",
		taskId, taskRev)

	m.mu.Lock()
	defer m.mu.Unlock()

	canceled := false

	taskHandlesNext := []*taskHandle(nil)

	for _, taskHandle := range m.tasks.taskHandles {
		task := taskHandle.task
		if task.ID == taskId {
			if taskRev != nil &&
				len(taskRev) > 0 &&
				!bytes.Equal(taskRev, task.Rev) {
				log.Errorf("ctl/manager: CancelTask,"+
					" taskId: %s, taskRev: %v, err: %v",
					taskId, taskRev, service.ErrConflict)
				return service.ErrConflict
			}

			if !task.IsCancelable {
				log.Errorf("ctl/manager: CancelTask,"+
					" taskId: %s, taskRev: %v, err: %v",
					taskId, taskRev, service.ErrNotSupported)
				return service.ErrNotSupported
			}

			if taskHandle.stop != nil {
				taskHandle.stop()
			} else {
				log.Printf("ctl/manager: CancelTask, taskId: %s, taskRev: %v,"+
					" nil taskHandle", taskId, taskRev)
			}

			canceled = true
		} else {
			taskHandlesNext = append(taskHandlesNext, taskHandle)
		}
	}

	if !canceled {
		log.Errorf("ctl/manager: CancelTask, taskId: %s, taskRev: %v, err: %v",
			taskId, taskRev, service.ErrNotFound)
		return service.ErrNotFound
	}

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: CancelTask, taskId: %s, taskRev: %v, done",
		taskId, taskRev)

	return nil
}

func isBalanced(ctl *Ctl, ctlTopology *CtlTopology) bool {
	if len(ctlTopology.PrevWarnings) > 0 {
		for _, w := range ctlTopology.PrevWarnings {
			if len(w) > 0 {
				return false
			}
		}
	}
	if len(ctlTopology.PrevErrs) != 0 {
		return false
	}

	lastRebStatus, err := ctl.optionsCtl.Manager.GetLastRebalanceStatus(false)
	if err != nil || lastRebStatus == cbgt.RebStarted {
		return false
	}

	return true
}

func (m *CtlMgr) GetCurrentTopology(haveTopologyRev service.Revision,
	cancelCh service.Cancel) (*service.Topology, error) {
	ctlTopology, err :=
		m.ctl.WaitGetTopology(haveTopologyRev, cancelCh)
	if err != nil {
		if err != service.ErrCanceled {
			log.Errorf("ctl/manager: GetCurrentTopology,"+
				" haveTopologyRev: %s, err: %v", haveTopologyRev, err)
		}

		return nil, err
	}

	rv := &service.Topology{
		Rev:   service.Revision([]byte(ctlTopology.Rev)),
		Nodes: []service.NodeID{},
	}

	for _, ctlNode := range ctlTopology.MemberNodes {
		rv.Nodes = append(rv.Nodes, service.NodeID(ctlNode.UUID))
	}

	// TODO: Need a proper IsBalanced computation.
	rv.IsBalanced = isBalanced(m.ctl, ctlTopology)

	for resourceName, resourceWarnings := range ctlTopology.PrevWarnings {
		aggregate := map[string]bool{}
		for _, resourceWarning := range resourceWarnings {
			if strings.HasPrefix(resourceWarning, "could not meet constraints") {
				aggregate["could not meet replication constraints"] = true
			} else {
				aggregate[resourceWarning] = true
			}
		}

		for resourceWarning := range aggregate {
			rv.Messages = append(rv.Messages,
				fmt.Sprintf("warning: resource: %q -- %s",
					resourceName, resourceWarning))
		}

		sort.Strings(rv.Messages)
	}

	for _, err := range ctlTopology.PrevErrs {
		rv.Messages = append(rv.Messages, fmt.Sprintf("error: %v", err))
	}

	m.lastTopologyM.Lock()
	m.lastTopology.Rev = rv.Rev
	same := reflect.DeepEqual(&m.lastTopology, rv)
	m.lastTopology = *rv
	m.lastTopologyM.Unlock()

	if !same {
		log.Printf("ctl/manager: GetCurrentTopology, haveTopologyRev: %s,"+
			" changed, rv: %+v", haveTopologyRev, rv)
	}

	return rv, nil
}

func (m *CtlMgr) PrepareTopologyChange(
	change service.TopologyChange) (err error) {
	log.Printf("ctl/manager: PrepareTopologyChange, change: %v", change)

	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		if err == nil {
			m.ctl.onSuccessfulPrepare(true)
		}
	}()

	// Possible for caller to not care about current topology, but
	// just wants to impose or force a topology change.
	if len(change.CurrentTopologyRev) > 0 &&
		string(change.CurrentTopologyRev) != m.ctl.GetTopology().Rev {
		log.Errorf("ctl/manager: PrepareTopologyChange, rev check, err: %v",
			service.ErrConflict)
		err = service.ErrConflict
		return err
	}

	for _, taskHandle := range m.tasks.taskHandles {
		if taskHandle.task.Type == service.TaskTypePrepared ||
			taskHandle.task.Type == service.TaskTypeRebalance {
			// NOTE: If there's an existing rebalance or preparation
			// task, even if it's done, then treat as a conflict, as
			// the caller should cancel them all first.
			log.Errorf("ctl/manager: PrepareTopologyChange, "+
				"task type check, err: %v", service.ErrConflict)
			err = service.ErrConflict
			return err
		}
	}

	revNum := m.allocRevNumLOCKED(0)

	taskHandlesNext := append([]*taskHandle(nil),
		m.tasks.taskHandles...)
	taskHandlesNext = append(taskHandlesNext,
		&taskHandle{
			startTime: time.Now(),
			task: &service.Task{
				Rev:              EncodeRev(revNum),
				ID:               "prepare:" + change.ID,
				Type:             service.TaskTypePrepared,
				Status:           service.TaskStatusRunning,
				IsCancelable:     true,
				Progress:         100.0, // Prepared born as 100.0 is ok.
				DetailedProgress: nil,
				Description:      "prepare topology change",
				ErrorMessage:     "",
				Extra: map[string]interface{}{
					"topologyChange": change,
				},
			},
		})

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	// if the current node is in keep list, checking further
	// for a reregister
	for _, node := range change.KeepNodes {
		if m.nodeInfo.NodeID == node.NodeInfo.NodeID {
			m.ctl.checkAndReregisterSelf(string(m.nodeInfo.NodeID))
		}
	}

	log.Printf("ctl/manager: PrepareTopologyChange, done")

	return nil
}

func (m *CtlMgr) StartTopologyChange(change service.TopologyChange) error {
	log.Printf("ctl/manager: StartTopologyChange, change: %v", change)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Possible for caller to not care about current topology, but
	// just wants to impose or force a topology change.
	if len(change.CurrentTopologyRev) > 0 &&
		string(change.CurrentTopologyRev) != m.ctl.GetTopology().Rev {
		log.Errorf("ctl/manager: StartTopologyChange, rev check, err: %v",
			service.ErrConflict)
		return service.ErrConflict
	}

	var err error

	started := false

	var taskHandlesNext []*taskHandle

	for _, th := range m.tasks.taskHandles {
		if th.task.Type == service.TaskTypeRebalance {
			log.Errorf("ctl/manager: StartTopologyChange,"+
				" task rebalance check, err: %v", service.ErrConflict)
			return service.ErrConflict
		}

		if th.task.Type == service.TaskTypePrepared {
			th, err = m.startTopologyChangeTaskHandleLOCKED(change)
			if err != nil {
				log.Errorf("ctl/manager: StartTopologyChange,"+
					" prepared, err: %v", err)
				return err
			}

			started = true
		}

		taskHandlesNext = append(taskHandlesNext, th)
	}

	if !started {
		return service.ErrNotFound
	}

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: StartTopologyChange, started")

	return nil
}

func (m *CtlMgr) startTopologyChangeTaskHandleLOCKED(
	change service.TopologyChange) (*taskHandle, error) {
	ctlChangeTopology := &CtlChangeTopology{
		Rev: string(change.CurrentTopologyRev),
	}

	switch change.Type {
	case service.TopologyChangeTypeRebalance:
		ctlChangeTopology.Mode = "rebalance"

	case service.TopologyChangeTypeFailover:
		ctlChangeTopology.Mode = "failover-hard"

	default:
		log.Warnf("ctl/manager: unknown change.Type: %v", change.Type)
		return nil, service.ErrNotSupported
	}

	for _, node := range change.KeepNodes {
		// TODO: What about node.RecoveryType?

		nodeUUID := string(node.NodeInfo.NodeID)

		ctlChangeTopology.MemberNodeUUIDs =
			append(ctlChangeTopology.MemberNodeUUIDs, nodeUUID)
	}

	for _, node := range change.EjectNodes {
		ctlChangeTopology.EjectNodeUUIDs =
			append(ctlChangeTopology.EjectNodeUUIDs, string(node.NodeID))
	}

	taskId := "rebalance:" + change.ID

	// cache for partition rebalance progress stats per node.
	// pindex => node => progress.
	pindexNodeProgressCache := make(map[string]map[string]float64)

	// The progressEntries is a map of pindex ->
	// source_partition -> node -> *rebalance.ProgressEntry.
	onProgress := func(maxNodeLen, maxPIndexLen int,
		seenNodes map[string]bool,
		seenNodesSorted []string,
		seenPIndexes map[string]bool,
		seenPIndexesSorted []string,
		progressEntries map[string]map[string]map[string]*rebalance.ProgressEntry,
		errs []error,
	) string {
		m.updateProgress(taskId, seenNodes, seenPIndexes, pindexNodeProgressCache,
			progressEntries, errs)

		if progressEntries == nil {
			return "DONE"
		}

		return rebalance.ProgressTableString(
			maxNodeLen, maxPIndexLen,
			seenNodes,
			seenNodesSorted,
			seenPIndexes,
			seenPIndexesSorted,
			progressEntries)
	}

	m.ctl.setTaskOrchestratorTo(true)

	ctlTopology, err := m.ctl.ChangeTopology(ctlChangeTopology, onProgress)
	if err != nil {
		return nil, err
	}

	revNum := m.allocRevNumLOCKED(m.tasks.revNum)

	th := &taskHandle{
		startTime: time.Now(),
		task: &service.Task{
			Rev:              EncodeRev(revNum),
			ID:               taskId,
			Type:             service.TaskTypeRebalance,
			Status:           service.TaskStatusRunning,
			IsCancelable:     true,
			Progress:         0.0,
			DetailedProgress: map[service.NodeID]float64{},
			Description:      "topology change",
			ErrorMessage:     "",
			Extra: map[string]interface{}{
				"topologyChange": change,
			},
		},
		stop: func() {
			log.Printf("ctl/manager: stop taskHandle, ctlTopology.Rev: %v",
				ctlTopology.Rev)

			m.ctl.StopChangeTopology(ctlTopology.Rev)
		},
	}

	return th, nil
}

func (m *CtlMgr) computeProgPercent(pe *rebalance.ProgressEntry,
	sourcePartitions map[string]map[string]*rebalance.ProgressEntry) float64 {
	totPct, avgPct := 0.0, -1.1
	numPct := 0
	if pe != nil {
		if sourcePartitions != nil {
			for _, nodes := range sourcePartitions {
				pex := nodes[pe.Node]
				if pex == nil || pex.WantUUIDSeq.UUID == "" {
					continue
				}

				if pex.WantUUIDSeq.Seq <= pex.CurrUUIDSeq.Seq {
					totPct = totPct + 1.0
					numPct = numPct + 1
					continue
				}

				n := pex.CurrUUIDSeq.Seq - pex.InitUUIDSeq.Seq
				d := pex.WantUUIDSeq.Seq - pex.InitUUIDSeq.Seq
				if d > 0 {
					pct := float64(n) / float64(d)
					totPct = totPct + pct
					numPct = numPct + 1
				}
			}
		}
	}
	if numPct > 0 {
		avgPct = totPct / float64(numPct)
	}
	return avgPct
}

func (m *CtlMgr) updateHibernationProgress(taskId string,
	progressEntries map[string]float64, errs []error) {
	var totalProgress float64
	if progressEntries != nil {
		var currTotalProgress float64
		for _, progress := range progressEntries {
			currTotalProgress += progress
		}
		totalProgress = currTotalProgress / float64(len(progressEntries))
	}
	taskProgressVal := taskProgress{
		taskId:         taskId,
		errs:           errs,
		progressExists: progressEntries != nil,
		progress:       totalProgress,
	}

	select {
	case m.taskProgressCh <- taskProgressVal:
		// NO-OP.
	default:
		// NO-OP, if the handleTaskProgress() goroutine is behind,
		// drop notifications rather than hold up the hibernation manager.
	}
}

// ------------------------------------------------

// The progressEntries is a map of...
// pindex -> sourcePartition -> node -> *ProgressEntry.
//
// The updateProgress() implementation must not block, in order to not
// block the invoking rebalancer.
func (m *CtlMgr) updateProgress(
	taskId string,
	seenNodes map[string]bool,
	seenPIndexes map[string]bool,
	pindexNodeProgressCache map[string]map[string]float64,
	progressEntries map[string]map[string]map[string]*rebalance.ProgressEntry,
	errs []error,
) {
	var progress float64
	if progressEntries != nil {
		for _, sourcePartitions := range progressEntries {
			for _, nodes := range sourcePartitions {
				for _, pex := range nodes {
					if pex == nil || pex.WantUUIDSeq.UUID == "" {
						continue
					}

					// skip the progress recomputations.
					if nodeProgMap, exists := pindexNodeProgressCache[pex.PIndex]; exists {
						if prog, ok := nodeProgMap[pex.Node]; ok && prog >= 1.0 {
							continue
						}
					}

					curProg := m.computeProgPercent(pex, sourcePartitions)
					if curProg > 0 || pex.TransferProgress > 0 {
						var t float64
						// file transfer progress is made to contribute to 80% of the rebalance
						// progress of a given partition and the rest by the seq number catchup.
						if pex.TransferProgress > 0 {
							t = .8 * pex.TransferProgress
							if curProg > 0 {
								t += .2 * curProg
							}
						} else {
							t = curProg
						}

						if nodeProgMap, exists := pindexNodeProgressCache[pex.PIndex]; !exists {
							nodeProgMap = make(map[string]float64)
							nodeProgMap[pex.Node] = t
							pindexNodeProgressCache[pex.PIndex] = nodeProgMap
						} else if v, ok := nodeProgMap[pex.Node]; !ok || v < t {
							nodeProgMap[pex.Node] = t
						}
					}
				}
			}
		}

		var pindexCount int
		totPct := 0.0
		for _, pindexProg := range pindexNodeProgressCache {
			for _, prog := range pindexProg {
				if prog > 0 {
					totPct += prog
					pindexCount++
				}
			}
		}

		// dynamically adjust the normalising factor.
		nfactor := m.ctl.movingPartitionsCount
		if nfactor < pindexCount {
			nfactor = pindexCount
		}

		if nfactor > 0 {
			progress = totPct / float64(nfactor)
		}
	}

	taskProgressVal := taskProgress{
		taskId:         taskId,
		errs:           errs,
		progressExists: progressEntries != nil,
		progress:       progress,
	}

	select {
	case m.taskProgressCh <- taskProgressVal:
		// NO-OP.
	default:
		// NO-OP, if the handleTaskProgress() goroutine is behind,
		// drop notifications rather than hold up the rebalancer.
	}
}

func (m *CtlMgr) handleTaskProgress(taskProgress taskProgress) {
	m.mu.Lock()
	defer m.mu.Unlock()

	updated := false

	var taskHandlesNext []*taskHandle

	for _, th := range m.tasks.taskHandles {
		if th.task.ID == taskProgress.taskId {
			if taskProgress.progressExists || len(taskProgress.errs) > 0 {
				revNum := m.allocRevNumLOCKED(0)

				taskNext := *th.task // Copy.
				taskNext.Rev = EncodeRev(revNum)
				taskNext.Progress = taskProgress.progress

				log.Printf("ctl/manager: revNum: %d, progress: %f",
					revNum, taskProgress.progress)

				// TODO: DetailedProgress.

				taskNext.ErrorMessage = ""
				for _, err := range taskProgress.errs {
					if len(taskNext.ErrorMessage) > 0 {
						taskNext.ErrorMessage = taskNext.ErrorMessage + "\n"
					}
					taskNext.ErrorMessage = taskNext.ErrorMessage + err.Error()
				}

				if len(taskProgress.errs) > 0 {
					taskNext.Status = service.TaskStatusFailed
				}

				taskHandlesNext = append(taskHandlesNext, &taskHandle{
					startTime: th.startTime,
					task:      &taskNext,
					stop:      th.stop,
				})
			}

			updated = true
		} else {
			taskHandlesNext = append(taskHandlesNext, th)
		}
	}

	if !updated {
		return
	}

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})
}

// parsePIndexName returns the "indexName_indexUUID", given an input
// pindexName that has a format that looks like
// "indexName_indexUUID_pindexSpecificSuffix", where the indexName can
// also have more underscores.
func parsePIndexName(pindexName string) string {
	uscoreLast := strings.LastIndex(pindexName, "_")
	if uscoreLast >= 0 {
		return pindexName[0:uscoreLast]
	}
	return ""
}

// ------------------------------------------------

func (m *CtlMgr) getTaskListLOCKED() *service.TaskList {
	rv := &service.TaskList{
		Rev:   EncodeRev(m.tasks.revNum),
		Tasks: []service.Task{},
	}

	for _, taskHandle := range m.tasks.taskHandles {
		rv.Tasks = append(rv.Tasks, *taskHandle.task)
	}

	return rv
}

// ------------------------------------------------

func (m *CtlMgr) updateTasksLOCKED(body func(tasks *tasks)) {
	body(&m.tasks)

	m.tasks.revNum = m.allocRevNumLOCKED(m.tasks.revNum)

	if m.tasksWaitCh != nil {
		close(m.tasksWaitCh)
		m.tasksWaitCh = nil
	}
}

// ------------------------------------------------

func (m *CtlMgr) allocRevNumLOCKED(prevRevNum uint64) uint64 {
	rv := prevRevNum + 1
	if rv < m.revNumNext {
		rv = m.revNumNext
	}
	m.revNumNext = rv + 1
	return rv
}

// ------------------------------------------------

func EncodeRev(revNum uint64) service.Revision {
	return []byte(fmt.Sprintf("%d", revNum))
}

func DecodeRev(b service.Revision) (uint64, error) {
	return strconv.ParseUint(string(b), 10, 64)
}

// ------------------------------------------------

type CtlManagerStatusHandler struct {
	m *CtlMgr
}

func NewCtlManagerStatusHandler(mgr *CtlMgr) *CtlManagerStatusHandler {
	return &CtlManagerStatusHandler{m: mgr}
}

func (h *CtlManagerStatusHandler) ServeHTTP(
	w http.ResponseWriter, req *http.Request) {
	rv := struct {
		Orchestrator bool   `json:"orchestrator"`
		Status       string `json:"status"`
	}{
		Status:       "ok",
		Orchestrator: h.m.ctl.isTaskOrchestrator(),
	}
	rest.MustEncode(w, rv)
}

// ------------------------------------------------

type CtlHibernationStatusHandler struct {
	m *CtlMgr
}

func NewCtlHibernationStatusHandler(mgr *CtlMgr) *CtlHibernationStatusHandler {
	return &CtlHibernationStatusHandler{m: mgr}
}

func (h *CtlHibernationStatusHandler) ServeHTTP(
	w http.ResponseWriter, req *http.Request) {
	rv := struct {
		HibernationPlanStatus bool   `json:"hibernationPlanPhase"`
		HibernationTaskType   string `json:"hibernationTaskType"`
	}{
		HibernationPlanStatus: h.m.ctl.checkHibernationPlanStatus(),
		HibernationTaskType:   h.m.ctl.hibernationTaskType(),
	}
	rest.MustEncode(w, rv)
}

// ------------------------------------------------

// DefragmentedUtilizationHook allows applications to register a
// callback to determine the projected "defragmented" utilization
// stats for the nodes belonging to the service. This should be
// set only during the init()'ialization phase of the process.
var DefragmentedUtilizationHook func(nodeDefs *cbgt.NodeDefs) (
	*service.DefragmentedUtilizationInfo, error)

func (m *CtlMgr) GetDefragmentedUtilization() (
	*service.DefragmentedUtilizationInfo, error) {
	if DefragmentedUtilizationHook != nil {
		nodeDefsKnown, _, err := cbgt.CfgGetNodeDefs(m.ctl.cfg, cbgt.NODE_DEFS_KNOWN)
		if err != nil {
			return nil, err
		}
		return DefragmentedUtilizationHook(nodeDefsKnown)
	}

	return nil, nil
}

// ------------------------------------------------

// PreparePause just updates the task lists with the prepared task
// type along with some basic validations.
func (m *CtlMgr) PreparePause(params service.PauseParams) (err error) {
	log.Printf("ctl/manager: PreparePause, params: %v", params)

	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		if err == nil {
			m.ctl.onSuccessfulPrepare(false)
		}
	}()

	for _, taskHandle := range m.tasks.taskHandles {
		if taskHandle.task.Type == service.TaskTypePrepared ||
			taskHandle.task.Type == service.TaskTypeBucketPause ||
			taskHandle.task.Type == service.TaskTypeBucketResume ||
			taskHandle.task.Type == service.TaskTypeRebalance {
			// NOTE: If there's an existing rebalance, preparation,
			// bucket pause/resume task, even if it's done, then treat
			// as a conflict, as the caller should cancel them all first.
			log.Errorf("ctl/manager: PreparePause, conflicts with task type: %s,"+
				" err: %v", taskHandle.task.Type, service.ErrConflict)
			err = service.ErrConflict
			return err
		}
	}

	err = m.ctl.optionsCtl.Manager.HibernationPrepareUtil(cbgt.HIBERNATE_TASK, params.Bucket,
		params.BlobStorageRegion, params.RateLimit, false)
	if err != nil {
		return fmt.Errorf("ctl/manager: failed in the prepare phase for bucket %s: %v",
			params.Bucket, err)
	}

	revNum := m.allocRevNumLOCKED(0)

	taskHandlesNext := append([]*taskHandle(nil),
		m.tasks.taskHandles...)
	taskHandlesNext = append(taskHandlesNext,
		&taskHandle{
			startTime: time.Now(),
			task: &service.Task{
				Rev:              EncodeRev(revNum),
				ID:               "prepare:" + params.ID,
				Type:             service.TaskTypePrepared,
				Status:           service.TaskStatusRunning,
				IsCancelable:     true,
				Progress:         100.0,
				DetailedProgress: nil,
				Description:      "prepare pause handler",
				ErrorMessage:     "",
				Extra: map[string]interface{}{
					"preparePause": params,
				},
			},
			stop: func() {
				log.Printf("ctl/manager: stop preparePause: %v",
					params)

				m.ctl.StopHibernationTask()
			},
		})

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: PreparePause, done")

	return nil
}

// PrepareResume just updates the task lists with the prepared task
// type along with some basic validations.
func (m *CtlMgr) PrepareResume(params service.ResumeParams) (err error) {
	log.Printf("ctl/manager: PrepareResume, params: %v", params)

	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		if err == nil {
			m.ctl.onSuccessfulPrepare(false)
		}
	}()

	for _, taskHandle := range m.tasks.taskHandles {
		if taskHandle.task.Type == service.TaskTypePrepared ||
			taskHandle.task.Type == service.TaskTypeBucketPause ||
			taskHandle.task.Type == service.TaskTypeBucketResume ||
			taskHandle.task.Type == service.TaskTypeRebalance {
			// NOTE: If there's an existing rebalance, preparation,
			// bucket pause/resume task, even if it's done, then treat
			// as a conflict, as the caller should cancel them all first.
			log.Errorf("ctl/manager: PrepareResume, conflicts with task type: %s,"+
				" err: %v", taskHandle.task.Type, service.ErrConflict)
			err = service.ErrConflict
			return err
		}
	}

	revNum := m.allocRevNumLOCKED(0)

	newTaskHandle := &taskHandle{
		startTime: time.Now(),
		task: &service.Task{
			Rev:              EncodeRev(revNum),
			ID:               "prepare:" + params.ID,
			Type:             service.TaskTypePrepared,
			Status:           service.TaskStatusRunning,
			IsCancelable:     true,
			Progress:         100.0,
			DetailedProgress: nil,
			Description:      "prepare resume handler",
			ErrorMessage:     "",
			Extra: map[string]interface{}{
				"prepareResume": params,
			},
		},
		stop: func() {
			log.Printf("ctl/manager: stop prepareResume: %v",
				params)

			m.ctl.StopHibernationTask()
		}}

	err = m.ctl.optionsCtl.Manager.HibernationPrepareUtil(cbgt.UNHIBERNATE_TASK, params.Bucket,
		params.BlobStorageRegion, params.RateLimit, params.DryRun)
	if err != nil {
		return fmt.Errorf("ctl/manager: failed in the prepare phase for bucket %s: %v",
			params.Bucket, err)
	}

	if params.DryRun {
		// Task marked as failed if the path is invalid.
		if !hibernate.CheckIfRemotePathIsValidHook(params.RemotePath) {
			newTaskHandle.task.Status = service.TaskStatusCannotResume
			newTaskHandle.task.ErrorMessage = "invalid remote path"
		}
	}

	taskHandlesNext := append([]*taskHandle(nil),
		m.tasks.taskHandles...)
	taskHandlesNext = append(taskHandlesNext, newTaskHandle)

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: PrepareResume, done")

	return nil
}

// Pause is the starting point for pause operation.
// It adds pause tasks to the tasks list and updates it.
func (m *CtlMgr) Pause(params service.PauseParams) error {
	log.Printf("ctl/manager: Pause, params: %v", params)

	m.mu.Lock()
	defer m.mu.Unlock()

	var taskHandlesNext []*taskHandle

	for _, th := range m.tasks.taskHandles {
		if th.task.Type == service.TaskTypeRebalance ||
			th.task.Type == service.TaskTypeBucketPause ||
			th.task.Type == service.TaskTypeBucketResume {
			log.Errorf("ctl/manager: Pause, conflicts with task type: %s,"+
				" err: %v", th.task.Type, service.ErrConflict)
			return service.ErrConflict
		}
	}

	th, err := m.pauseTaskHandleLOCKED(params)
	if err != nil {
		log.Errorf("ctl/manager: Pause, err: %v", err)
		return err

	}

	taskHandlesNext = append(taskHandlesNext, th)

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: Pause, started")

	return nil
}

func (m *CtlMgr) pauseTaskHandleLOCKED(
	params service.PauseParams) (*taskHandle, error) {
	log.Printf("ctl/manager: pauseTaskHandleLOCKED, params: %v", params)

	taskId := string(hibernate.OperationType(cbgt.HIBERNATE_TASK)) + ":" + params.ID

	onProgress := func(progressEntries map[string]float64, errs []error) {
		m.updateHibernationProgress(taskId, progressEntries, errs)
	}

	params.RemotePath = string(hibernate.OperationType(cbgt.HIBERNATE_TASK)) + ":" +
		params.RemotePath
	err := m.ctl.startHibernation(false, params.Bucket, params.RemotePath,
		hibernate.OperationType(cbgt.HIBERNATE_TASK), onProgress)
	if err != nil {
		return nil, err
	}

	revNum := m.allocRevNumLOCKED(m.tasks.revNum)

	th := &taskHandle{
		startTime: time.Now(),
		task: &service.Task{
			Rev:              EncodeRev(revNum),
			ID:               taskId,
			Type:             service.TaskTypeBucketPause,
			Status:           service.TaskStatusRunning,
			IsCancelable:     true,
			Progress:         0.0,
			DetailedProgress: map[service.NodeID]float64{},
			Description:      "pause change",
			ErrorMessage:     "",
			Extra: map[string]interface{}{
				"pause": params,
			},
		},
		stop: func() {
			log.Printf("ctl/manager: stop Pause: %v", params)

			m.ctl.optionsCtl.Manager.ResetBucketTrackedForHibernation()
			m.ctl.StopHibernationTask()
		},
	}

	return th, nil
}

func (m *CtlMgr) Resume(params service.ResumeParams) error {
	log.Printf("ctl/manager: Resume, params: %v", params)

	m.mu.Lock()
	defer m.mu.Unlock()

	var taskHandlesNext []*taskHandle

	for _, th := range m.tasks.taskHandles {
		if th.task.Type == service.TaskTypeRebalance ||
			th.task.Type == service.TaskTypeBucketPause ||
			th.task.Type == service.TaskTypeBucketResume {
			log.Errorf("ctl/manager: Resume, conflicts with task type: %s,"+
				" err: %v", th.task.Type, service.ErrConflict)
			return service.ErrConflict
		}
	}

	th, err := m.resumeTaskHandleLOCKED(params)
	if err != nil {
		log.Errorf("ctl/manager: Resume, err: %v", err)
		return err

	}

	taskHandlesNext = append(taskHandlesNext, th)

	m.updateTasksLOCKED(func(s *tasks) {
		s.taskHandles = taskHandlesNext
	})

	log.Printf("ctl/manager: Resume, started")

	return nil
}

func (m *CtlMgr) resumeTaskHandleLOCKED(
	params service.ResumeParams) (*taskHandle, error) {
	log.Printf("ctl/manager: resumeTaskHandleLOCKED, params: %v", params)

	taskId := string(hibernate.OperationType(cbgt.UNHIBERNATE_TASK)) + ":" + params.ID

	revNum := m.allocRevNumLOCKED(m.tasks.revNum)
	th := &taskHandle{
		startTime: time.Now(),
		task: &service.Task{
			Rev:              EncodeRev(revNum),
			ID:               taskId,
			Type:             service.TaskTypeBucketResume,
			Status:           service.TaskStatusRunning,
			IsCancelable:     true,
			Progress:         0.0,
			DetailedProgress: map[service.NodeID]float64{},
			Description:      "resume change",
			ErrorMessage:     "",
			Extra: map[string]interface{}{
				"resume": params,
			},
		},
		stop: func() {
			log.Printf("ctl/manager: stop Resume: %v", params)

			m.ctl.optionsCtl.Manager.ResetBucketTrackedForHibernation()
			m.ctl.StopHibernationTask()
		},
	}

	onProgress := func(progressEntries map[string]float64, errs []error) {
		m.updateHibernationProgress(taskId, progressEntries, errs)
	}

	params.RemotePath = string(hibernate.OperationType(cbgt.UNHIBERNATE_TASK)) + ":" +
		params.RemotePath
	err := m.ctl.startHibernation(params.DryRun, params.Bucket, params.RemotePath,
		hibernate.OperationType(cbgt.UNHIBERNATE_TASK), onProgress)
	if err != nil {
		return nil, err
	}

	return th, nil
}
