// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"math/rand"
	"sort"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout       int
	randomElectionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).
	raftLog := newLog(c.Storage)
	initialState, confState, err := raftLog.storage.InitialState()
	if err != nil {
		panic(err)
	}
	r := &Raft{
		id:               c.ID,
		Term:             initialState.GetTerm(),
		Vote:             initialState.GetVote(),
		RaftLog:          newLog(c.Storage),
		Prs:              make(map[uint64]*Progress),
		votes:            make(map[uint64]bool),
		Lead:             0,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		heartbeatElapsed: 0,
		electionElapsed:  0,
		leadTransferee:   0,
		PendingConfIndex: 0,
	}
	if len(c.peers) == 0 || c.peers == nil {
		c.peers = confState.Nodes
	}
	lastIndex := r.RaftLog.LastIndex()
	for _, pr := range c.peers {
		if pr == r.id {
			r.Prs[pr] = &Progress{lastIndex, lastIndex + 1}
		} else {
			r.Prs[pr] = &Progress{0, lastIndex + 1}
		}
		r.votes[pr] = false
	}
	r.becomeFollower(0, 0)
	r.Vote = initialState.GetVote()
	r.RaftLog.committed = initialState.GetCommit()
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	if c.Applied > 0 {
		r.RaftLog.applied = c.Applied
	}
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	prevIndex := r.Prs[to].Next - 1
	prevLogTerm, err := r.RaftLog.Term(prevIndex)
	if err != nil {
		if err == ErrCompacted {
			r.sendSnapshot(to)
			return false
		}
		panic(err)
	}
	entries := make([]*pb.Entry, 0)
	for i := r.RaftLog.toSliceIndex(prevIndex + 1); i < len(r.RaftLog.entries); i++ {
		entries = append(entries, &r.RaftLog.entries[i])
	}
	m := &pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Index:   prevIndex,
		LogTerm: prevLogTerm,
		Entries: entries,
		Commit:  r.RaftLog.committed,
	}
	r.msgs = append(r.msgs, *m)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	m := &pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		LogTerm: 0,
		Index:   0,
		Entries: nil,
	}
	r.msgs = append(r.msgs, *m)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		r.electionElapsed++
		if r.electionElapsed >= r.randomElectionTimeout {
			r.electionElapsed = 0
			// 选举超时逻辑
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				panic(err)
			}
		}
	case StateCandidate:
		r.electionElapsed++
		if r.electionElapsed >= r.randomElectionTimeout {
			r.electionElapsed = 0
			// 选举超时逻辑
			err := r.Step(pb.Message{MsgType: pb.MessageType_MsgHup})
			if err != nil {
				panic(err)
			}
		}
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed >= r.heartbeatTimeout {
			r.heartbeatElapsed = 0
			for peer := range r.Prs {
				if peer != r.id {
					r.sendHeartbeat(peer)
				}
			}
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.State = StateFollower
	r.Term = term
	r.Lead = lead
	r.Vote = 0
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.Term++
	r.Lead = 0
	r.Vote = r.id
	for k := range r.votes {
		r.votes[k] = false
	}
	r.votes[r.id] = true
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.State = StateLeader
	r.Lead = r.id
	r.heartbeatElapsed = 0
	for peer := range r.Prs {
		if peer != r.id {
			r.Prs[peer].Match = 0
			r.Prs[peer].Next = r.RaftLog.LastIndex() + 1
		} else {
			r.Prs[peer].Match = r.RaftLog.LastIndex() + 1
			r.Prs[peer].Next = r.RaftLog.LastIndex() + 2
		}
	}
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{
		Term:  r.Term,
		Index: r.RaftLog.LastIndex() + 1,
	})
	for peer := range r.Prs {
		if peer != r.id {
			r.sendAppend(peer)
		}
	}
	if len(r.Prs) == 1 {
		r.RaftLog.committed = r.Prs[r.id].Match
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	if _, ok := r.Prs[r.id]; !ok && m.MsgType == pb.MessageType_MsgTimeoutNow {
		return nil
	}
	if m.Term > r.Term {
		r.leadTransferee = None
		r.becomeFollower(m.Term, None)
	}
	switch r.State {
	case StateFollower:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
			r.handleStartElection(m)
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
		case pb.MessageType_MsgBeat:
		case pb.MessageType_MsgPropose:
		case pb.MessageType_MsgAppendResponse:
		case pb.MessageType_MsgHeartbeatResponse:
		case pb.MessageType_MsgTransferLeader:
			if r.Lead != None {
				m.To = r.Lead
				r.msgs = append(r.msgs, m)
			}
		case pb.MessageType_MsgTimeoutNow:
			r.handleStartElection(m)
		}
	case StateCandidate:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
			r.handleStartElection(m)
		case pb.MessageType_MsgAppend:
			if m.Term == r.Term {
				r.becomeFollower(m.Term, m.From)
			}
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
			r.handleRequestVoteResponse(m)
		case pb.MessageType_MsgBeat:
		case pb.MessageType_MsgPropose:
		case pb.MessageType_MsgAppendResponse:
		case pb.MessageType_MsgHeartbeatResponse:
		case pb.MessageType_MsgTransferLeader:
			if r.Lead != None {
				m.To = r.Lead
				r.msgs = append(r.msgs, m)
			}
		case pb.MessageType_MsgTimeoutNow:
		}
	case StateLeader:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgSnapshot:
			r.handleSnapshot(m)
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
		case pb.MessageType_MsgBeat:
			for peer := range r.Prs {
				if peer != r.id {
					r.sendHeartbeat(peer)
				}
			}
		case pb.MessageType_MsgPropose:
			r.handlePropose(m)
		case pb.MessageType_MsgAppendResponse:
			r.handlerAppendEntriesResponse(m)
		case pb.MessageType_MsgHeartbeatResponse:
			r.handleHeartbeatResponse(m)
		case pb.MessageType_MsgTransferLeader:
			r.handleTransferLeader(m)
		case pb.MessageType_MsgTimeoutNow:
		}
	}
	return nil
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	if m.Term != 0 && m.Term < r.Term {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgAppendResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
			Index:   0,
			LogTerm: 0,
		})
		return
	}
	r.electionElapsed = 0
	r.Lead = m.From
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	if m.Index > r.RaftLog.LastIndex() {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgAppendResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
			Index:   r.RaftLog.LastIndex() + 1,
		})
		return
	}
	if m.Index >= r.RaftLog.LogIndex {
		logTerm, err := r.RaftLog.Term(m.Index)
		if err != nil {
			panic(err)
		}
		if logTerm != m.LogTerm {
			var index uint64 = r.RaftLog.LogIndex + uint64(sort.Search(int(m.Index-r.RaftLog.LogIndex+1), func(i int) bool {
				return r.RaftLog.entries[i].Term == logTerm
			}))
			r.msgs = append(r.msgs, pb.Message{
				MsgType: pb.MessageType_MsgAppendResponse,
				From:    r.id,
				To:      m.From,
				Term:    r.Term,
				Reject:  true,
				Index:   index,
				LogTerm: logTerm,
			})
			return
		}
	}
	for i, entry := range m.Entries {
		if entry.Index < r.RaftLog.LogIndex {
			continue
		}
		if entry.Index > r.RaftLog.LastIndex() {
			for j := i; j < len(m.Entries); j++ {
				r.RaftLog.entries = append(r.RaftLog.entries, *m.Entries[j])
			}
			break
		} else {
			logTerm, err := r.RaftLog.Term(entry.Index)
			if err != nil {
				panic(err)
			}
			if logTerm != entry.Term {
				idx := r.RaftLog.toSliceIndex(entry.Index)
				r.RaftLog.entries[idx] = *entry
				r.RaftLog.entries = r.RaftLog.entries[:idx+1]
				r.RaftLog.stabled = min(r.RaftLog.stabled, entry.Index-1)
			}
		}
	}
	if m.Commit > r.RaftLog.committed {
		r.RaftLog.committed = min(m.Commit, m.Index+uint64(len(m.Entries)))
	}
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
		Index:   r.RaftLog.LastIndex(),
		Reject:  false,
	})
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	if m.Term != 0 && m.Term < r.Term {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgHeartbeatResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
		})
		return
	}
	if m.Term == r.Term {
		r.electionElapsed = 0
		r.Lead = m.From
		r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
		r.heartbeatElapsed = 0
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgHeartbeatResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  false,
		})
	}
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	meta := m.Snapshot.Metadata
	if meta.Index <= r.RaftLog.committed {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgAppendResponse,
			From:    r.id,
			To:      m.From,
			Term:    0,
			Reject:  false,
			Index:   r.RaftLog.committed,
		})
		return
	}
	r.becomeFollower(max(r.Term, m.Term), m.From)
	first := meta.Index + 1
	if len(r.RaftLog.entries) > 0 {
		r.RaftLog.entries = nil
	}
	r.RaftLog.LogIndex = first
	r.RaftLog.applied = meta.Index
	r.RaftLog.committed = meta.Index
	r.RaftLog.stabled = meta.Index
	r.Prs = make(map[uint64]*Progress)
	for _, peer := range meta.ConfState.Nodes {
		r.Prs[peer] = &Progress{}
	}
	r.RaftLog.pendingSnapshot = m.Snapshot
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    r.id,
		To:      m.From,
		Term:    0,
		Reject:  false,
		Index:   r.RaftLog.LastIndex(),
	})
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}

func (r *Raft) handleRequestVote(m pb.Message) {
	//如果候选者的任期小于自己的任期，拒绝投票
	if m.Term != 0 && m.Term < r.Term {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgRequestVoteResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
		})
		return
	}
	//如果自己尚未投票或者已经投票给了该候选者，且候选者的日志比自己新，投票给该候选者
	if r.Vote == 0 || r.Vote == m.From {
		lastIndex := r.RaftLog.LastIndex()
		lastLogTerm, _ := r.RaftLog.Term(lastIndex)
		if lastLogTerm > m.LogTerm || lastLogTerm == m.LogTerm && lastIndex > m.Index {
			r.msgs = append(r.msgs, pb.Message{
				MsgType: pb.MessageType_MsgRequestVoteResponse,
				From:    r.id,
				To:      m.From,
				Term:    r.Term,
				Reject:  true,
			})
			return
		}
		//如果候选者的日志比自己新，投票给该候选者
		r.Vote = m.From
		r.electionElapsed = 0
		r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgRequestVoteResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  false,
		})
	} else {
		r.msgs = append(r.msgs, pb.Message{
			MsgType: pb.MessageType_MsgRequestVoteResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
		})
	}
}

func (r *Raft) handleRequestVoteResponse(m pb.Message) {
	if m.Term != 0 && m.Term < r.Term {
		return
	}
	//判断自己是否被拒绝
	if m.Reject {
		r.votes[m.From] = false
	} else {
		r.votes[m.From] = true
	}
	count := 0
	//统计投票结果
	for _, vote := range r.votes {
		if vote {
			count++
		}
	}
	//如果超过半数的节点投票给自己，成为leader
	if count > len(r.Prs)/2 {
		r.becomeLeader()
	} else if len(r.votes)-count > len(r.Prs)/2 {
		r.becomeFollower(r.Term, None)
	}
}

func (r *Raft) handlePropose(m pb.Message) {
	if r.leadTransferee != None {
		return
	} else {
		lastIndex := r.RaftLog.LastIndex()
		entries := m.Entries
		for i, entry := range entries {
			entry.Term = r.Term
			entry.Index = lastIndex + uint64(i) + 1
			if entry.EntryType == pb.EntryType_EntryConfChange {
				if r.PendingConfIndex != None {
					continue
				}
				r.PendingConfIndex = entry.Index
			}
			r.RaftLog.entries = append(r.RaftLog.entries, *entry)
		}
		r.Prs[r.id].Match = r.RaftLog.LastIndex()
		r.Prs[r.id].Next = r.Prs[r.id].Match + 1
		for peer := range r.Prs {
			if peer != r.id {
				r.sendAppend(peer)
			}
		}
		if len(r.Prs) == 1 {
			r.RaftLog.committed = r.Prs[r.id].Match
		}
	}
}

func (r *Raft) handlerAppendEntriesResponse(m pb.Message) {
	if m.Term != 0 && m.Term < r.Term {
		return
	}
	if m.Reject {
		if m.Index == 0 {
			return
		}
		index := m.Index
		logTerm := m.LogTerm
		l := r.RaftLog
		sliceIndex := sort.Search(len(l.entries),
			func(i int) bool { return l.entries[i].Term > logTerm })
		if sliceIndex > 0 && l.entries[sliceIndex-1].Term == logTerm {
			index = uint64(sliceIndex) + l.LogIndex
		}
		r.Prs[m.From].Next = index
		r.sendAppend(m.From)
		return
	}
	if m.Index > r.Prs[m.From].Match {
		r.Prs[m.From].Match = m.Index
		r.Prs[m.From].Next = m.Index + 1
		r.updateCommit()
		if m.From == r.leadTransferee && m.Index == r.RaftLog.LastIndex() {
			r.msgs = append(r.msgs, pb.Message{
				MsgType: pb.MessageType_MsgTimeoutNow,
				From:    r.id,
				To:      m.From,
			})
			r.leadTransferee = None
		}
	}
}

func (r *Raft) updateCommit() {
	matchIndexs := make(uint64Slice, len(r.Prs))
	i := 0
	for _, pr := range r.Prs {
		matchIndexs[i] = pr.Match
		i++
	}
	sort.Sort(matchIndexs)
	N := matchIndexs[(len(r.Prs)-1)/2]
	logTerm, _ := r.RaftLog.Term(N)
	if N > r.RaftLog.committed && logTerm == r.Term {
		r.RaftLog.committed = N
		for peer := range r.Prs {
			if peer != r.id {
				r.sendAppend(peer)
			}
		}
	}
}

func (r *Raft) handleHeartbeatResponse(m pb.Message) {
	if r.Prs[m.From].Match < r.RaftLog.LastIndex() {
		r.sendAppend(m.From)
	}
}

func (r *Raft) handleStartElection(m pb.Message) {
	r.becomeCandidate()
	r.Vote = r.id
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true
	//更改选举超时时间
	r.heartbeatElapsed = 0
	r.randomElectionTimeout = r.electionTimeout + rand.Intn(r.electionTimeout)
	//如果只有自己一个节点，直接成为leader
	if len(r.Prs) == 1 {
		r.becomeLeader()
	}
	//向其他节点发送投票请求
	for peer := range r.Prs {
		if peer != r.id {
			r.sendRequestVote(peer)
		}
	}
}

func (r *Raft) sendRequestVote(peer uint64) {
	logTerm, _ := r.RaftLog.Term(r.RaftLog.LastIndex())
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		From:    r.id,
		To:      peer,
		Term:    r.Term,
		LogTerm: logTerm,
		Index:   r.RaftLog.LastIndex(),
	})
}

func (r *Raft) sendSnapshot(to uint64) {
	snapshot, err := r.RaftLog.storage.Snapshot()
	if err != nil {
		return
	}
	msg := pb.Message{
		MsgType:  pb.MessageType_MsgSnapshot,
		From:     r.id,
		To:       to,
		Term:     r.Term,
		Snapshot: &snapshot,
	}
	r.msgs = append(r.msgs, msg)
	r.Prs[to].Next = snapshot.Metadata.Index + 1
}

func (r *Raft) handleTransferLeader(m pb.Message) {
	if m.From == r.id {
		return
	}
	if r.leadTransferee != None && r.leadTransferee == m.From {
		return
	}
	if _, ok := r.Prs[m.From]; !ok {
		return
	}
	r.leadTransferee = m.From
	if r.Prs[m.From].Match == r.RaftLog.LastIndex() {
		msg := pb.Message{
			MsgType: pb.MessageType_MsgTimeoutNow,
			From:    r.id,
			To:      m.From,
		}
		r.msgs = append(r.msgs, msg)
	} else {
		r.sendAppend(m.From)
	}
}
