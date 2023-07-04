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
	electionTimeout int
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
		Term:             initialState.Term,
		Vote:             initialState.Vote,
		RaftLog:          newLog(c.Storage),
		Prs:              make(map[uint64]*Progress),
		State:            StateFollower,
		votes:            make(map[uint64]bool),
		msgs:             make([]pb.Message, 0),
		Lead:             0,
		heartbeatTimeout: c.HeartbeatTick,
		electionTimeout:  c.ElectionTick,
		heartbeatElapsed: 0,
		electionElapsed:  0,
		leadTransferee:   0,
		PendingConfIndex: 0,
	}
	if len(c.peers) == 0 {
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
	r.becomeFollower(r.Term, 0)
	return r
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	if r.Lead != r.id {
		return false
	}
	m := &pb.Message{
		MsgType: pb.MessageType_MsgAppend,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Index:   r.RaftLog.LastIndex() + 1,
		LogTerm: r.Term,
		Entries: make([]*pb.Entry, 0),
		Commit:  r.RaftLog.committed,
	}
	for i := r.Prs[to].Next; i <= r.RaftLog.LastIndex(); i++ {
		entry := r.RaftLog.entries[i]
		m.Entries = append(m.Entries, &entry)
	}
	r.msgs = append(r.msgs, *m)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	if r.Lead != r.id {
		return
	}
	m := &pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.id,
		To:      to,
		Term:    r.Term,
		Commit:  r.RaftLog.committed,
		LogTerm: r.Term,
		Index:   r.RaftLog.LastIndex() + 1,
		Entries: make([]*pb.Entry, 0),
	}
	r.msgs = append(r.msgs, *m)
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		r.electionElapsed++
		if r.electionElapsed > r.electionTimeout {
			// 选举超时逻辑
			r.becomeCandidate()
			r.electionElapsed = 0
		}
	case StateCandidate:
		r.electionElapsed++
		if r.electionElapsed > r.electionTimeout {
			// 选举超时逻辑
			r.becomeCandidate()
			r.electionElapsed = 0
		}
	case StateLeader:
		r.heartbeatElapsed++
		if r.heartbeatElapsed > r.heartbeatTimeout {
			for peer := range r.Prs {
				r.sendHeartbeat(peer)
			}
			r.heartbeatElapsed = 0
			r.electionElapsed = 0
		}
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	if term > r.Term {
		r.Vote = 0
	}
	r.State = StateFollower
	r.Term = term
	r.Lead = lead
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	for k := range r.votes {
		r.votes[k] = false
	}
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	r.State = StateCandidate
	r.Term++
	r.Lead = 0
	r.Vote = r.id
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
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
	r.electionElapsed = 0
	r.heartbeatElapsed = 0
	r.RaftLog.entries = append(r.RaftLog.entries, pb.Entry{
		Term:  r.Term,
		Index: r.RaftLog.LastIndex() + 1,
	})
	for peer := range r.Prs {
		r.Prs[peer].Match = 0
		r.Prs[peer].Next = r.RaftLog.LastIndex() + 1
		r.sendAppend(peer)
	}
	err := r.Step(pb.Message{
		MsgType: pb.MessageType_MsgPropose,
		From:    r.id,
		To:      r.id,
		Term:    r.Term,
		Entries: []*pb.Entry{
			{
				Term:  r.Term,
				Index: r.RaftLog.LastIndex() + 1,
			},
		},
	})
	if err != nil {
		panic(err)
	}
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
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
		case pb.MessageType_MsgTimeoutNow:
		}
	case StateCandidate:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
			r.handleStartElection(m)
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
			r.handleHeartbeat(m)
		case pb.MessageType_MsgSnapshot:
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
			r.handleRequestVoteResponse(m)
		case pb.MessageType_MsgBeat:
		case pb.MessageType_MsgPropose:
		case pb.MessageType_MsgAppendResponse:
		case pb.MessageType_MsgHeartbeatResponse:
		case pb.MessageType_MsgTransferLeader:
		case pb.MessageType_MsgTimeoutNow:
		}
	case StateLeader:
		switch m.MsgType {
		case pb.MessageType_MsgHup:
		case pb.MessageType_MsgAppend:
			r.handleAppendEntries(m)
		case pb.MessageType_MsgHeartbeat:
		case pb.MessageType_MsgSnapshot:
		case pb.MessageType_MsgRequestVote:
			r.handleRequestVote(m)
		case pb.MessageType_MsgRequestVoteResponse:
		case pb.MessageType_MsgBeat:
			for peer := range r.Prs {
				r.sendHeartbeat(peer)
			}
		case pb.MessageType_MsgPropose:
			r.handlePropose(m)
		case pb.MessageType_MsgAppendResponse:
			r.handlerAppendEntriesResponse(m)
		case pb.MessageType_MsgHeartbeatResponse:
			r.handleHeartbeatResponse(m)
		case pb.MessageType_MsgTransferLeader:
		case pb.MessageType_MsgTimeoutNow:
		}
	}
	return nil
}

// handleAppendEntries handle AppendEntries RPC request
func (r *Raft) handleAppendEntries(m pb.Message) {
	// Your Code Here (2A).
	r.Step(pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
	})
}

// handleHeartbeat handle Heartbeat RPC request
func (r *Raft) handleHeartbeat(m pb.Message) {
	// Your Code Here (2A).
	r.Step(pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
	})
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
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
	if m.Term < r.Term {
		r.Step(pb.Message{
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
		//如果候选者的日志比自己新，投票给该候选者
		if r.Term < m.Term {
			r.becomeFollower(m.Term, 0)
		}
		r.Vote = m.From
		r.Step(pb.Message{
			MsgType: pb.MessageType_MsgRequestVoteResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  false,
		})
	} else {
		r.Step(pb.Message{
			MsgType: pb.MessageType_MsgRequestVoteResponse,
			From:    r.id,
			To:      m.From,
			Term:    r.Term,
			Reject:  true,
		})
	}
}

func (r *Raft) handleRequestVoteResponse(m pb.Message) {
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
	}
}

func (r *Raft) handlePropose(m pb.Message) {
	if r.Lead != r.id {
		if r.leadTransferee != None {
			panic(ErrProposalDropped)
		} else {
			r.RaftLog.appendEntry(&pb.Entry{
				EntryType: pb.EntryType_EntryNormal,
				Data:      m.Entries[0].Data,
			})
			for peer := range r.Prs {
				r.sendAppend(peer)
			}
			if len(r.Prs) == 1 {
				r.RaftLog.committed = r.RaftLog.LastIndex()
			}
		}
	} else {
		panic(ErrProposalDropped)
	}
}

func (r *Raft) handlerAppendEntriesResponse(m pb.Message) {
	if m.Reject {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, m.From)
		} else {
			r.Prs[m.From].Next--
			r.sendAppend(m.From)
		}
	} else {
		r.Prs[m.From].Next = m.Index + 1
		r.Prs[m.From].Match = m.Index
		r.updateCommit()
	}
}

func (r *Raft) updateCommit() {
	matchIndexs := make([]uint64, len(r.Prs))
	i := 0
	for _, pr := range r.Prs {
		matchIndexs[i] = pr.Match
		i++
	}
	sort.Slice(matchIndexs, func(i, j int) bool {
		return matchIndexs[i] < matchIndexs[j]
	})
	N := matchIndexs[len(r.Prs)/2]
	if N > r.RaftLog.committed && r.RaftLog.entries[N].Term == r.Term {
		r.RaftLog.committed = N
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
	r.Term++
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true
	for peer := range r.Prs {
		r.sendRequestVote(peer)
	}
}

func (r *Raft) sendRequestVote(peer uint64) {
	logIndex, _ := r.RaftLog.Term(r.RaftLog.LastIndex())
	r.msgs = append(r.msgs, pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		From:    r.id,
		To:      peer,
		Term:    r.Term,
		LogTerm: logIndex,
		Index:   r.RaftLog.LastIndex(),
	})
}
