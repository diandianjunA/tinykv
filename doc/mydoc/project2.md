# raft

## project2A

1. 实现raft基本功能
    - 领导选举
    - 日志同步

2. 与上层交互的接口

### 初始化

Raft 结构体

```go
type Raft struct {
	Term uint64
	Vote uint64
	RaftLog *RaftLog
	Prs map[uint64]*Progress
	State StateType
	votes map[uint64]bool
	msgs []pb.Message
	Lead uint64
	heartbeatTimeout int
	electionTimeout       int
	randomElectionTimeout int
	heartbeatElapsed int
	electionElapsed int
	leadTransferee uint64
	PendingConfIndex uint64
}
```

- `id uint64`：表示Raft节点的唯一标识符。
- `Term uint64`：表示Raft节点的当前任期。
- `Vote uint64`：存储在当前任期中获得选票的节点的ID。
- `RaftLog *RaftLog`：指向Raft日志，其中包含已提交和未提交的条目。
- `Prs map[uint64]*Progress`：存储每个集群中同行的日志复制进度。
- `State StateType`：表示Raft节点的角色（如领导者、追随者、候选人）。
- `votes map[uint64]bool`：跟踪Raft节点在选举中收到的选票。
- `msgs []pb.Message`：表示Raft节点需要发送的消息。
- `Lead uint64`：存储当前领导者的ID。
- `heartbeatTimeout int`：指定发送心跳的间隔。
- `electionTimeout int`：表示选举间隔的基准值。
- `randomElectionTimeout int`：表示随机化的选举超时时间。
- `heartbeatElapsed int`：跟踪自上次心跳超时以来的时钟周期数。
- `electionElapsed int`：跟踪自上次选举超时或从领导者接收到有效消息以来的时钟周期数。
- `leadTransferee uint64`：表示在领导者转移过程中目标转移的领导者的ID。
- `PendingConfIndex uint64`：存储日志中最新待处理配置更改的索引。

`randomElectionTimeout int`要在以下几个情况下重置
成为候选人（Candidate）：当节点从跟随者（Follower）状态转变为候选人状态时

收到有效的领导者（Leader）消息：当节点处于跟随者状态，并且接收到有效的来自当前领导者的消息时

选举超时时间到期：每个节点在成为候选人之前都会设置一个随机的选举超时时间范围。如果选举超时时间到期而节点仍然是跟随者状态，它会转变为候选人并开始选举过程。在这种情况下，节点会重置随机选举超时时间，以便在下一次选举中使用。

RaftLog 结构体

```go
type RaftLog struct {
	storage Storage
    committed uint64
    applied uint64
    stabled uint64
	entries []pb.Entry
	pendingSnapshot *pb.Snapshot
	LogIndex uint64
}
```

- `storage Storage`：存储了从上一个快照之后的所有稳定的日志条目。
- `committed uint64`：已知在大多数节点的稳定存储中的最高日志位置。
- `applied uint64`：应用程序被指示应用到其状态机的最高日志位置。约束条件：applied <= committed。
- `stabled uint64`：已持久化到存储中的索引小于等于 stabled 的日志条目。它用于记录尚未由存储持久化的日志条目。每次处理 Ready 时，未稳定的日志将被包含在其中。
- `entries []pb.Entry`：尚未被压缩的所有日志条目。
- `pendingSnapshot *pb.Snapshot`：传入的不稳定快照（如果有的话）。在 2C 阶段使用。
- `LogIndex uint64`：存储中的第一个日志的索引。初始化为storage.FirstIndex()

committed 表示已经达成共识并持久化的日志位置，用于确定哪些日志已经被提交，可以进行状态机的应用。

applied 表示应用程序已经将日志应用到其状态机的位置，用于跟踪状态机的应用进度，以保持节点之间的一致性。

stabled 表示已经稳定存储到持久化介质的日志位置，用于确定哪些日志已经持久化，可以从内存中释放。

![image-20201229162238096](./img/project2_1.png)

### tick() 函数

tick() 函数是一个逻辑时钟

Follower维护一个electionElapsed计数器，当electionElapsed超过electionTimeout时，Follower会变成Candidate，发起一次新的选举。candidate会重新发起一次选举

leader会发送心跳包，维持自己的leader地位

### Step() 函数

用于处理消息请求，根据消息的类型和当前状态，执行相应的操作。

从网络接收到来自其他节点的消息：当节点作为网络的一部分，接收到来自其他节点的消息时，网络层会将消息传递给对应的 Raft 节点的 Step 函数进行处理。

但也有可能是本地的消息，比如本地的定时器超时，也会调用 Step 函数。

```go
    MsgHup = 0;
    MsgBeat = 1;
    MsgPropose = 2;
    MsgAppend = 3;
    MsgAppendResponse = 4;
    MsgRequestVote = 5;
    MsgRequestVoteResponse = 6;
    MsgSnapshot = 7;
    MsgHeartbeat = 8;
    MsgHeartbeatResponse = 9;
    MsgTransferLeader = 11;
    MsgTimeoutNow = 12;
```

- MsgHup 发起选举：当节点检测到选举超时时，它可以通过调用 Step 函数并传递 MessageType_MsgHup 消息来发起新的选举过程。

- MsgBeat 发送心跳：领导者节点可以定期发送心跳消息以维持其领导地位和通知追随者其仍然活跃。领导者节点可以通过调用 Step 函数并传递 MessageType_MsgBeat 消息来触发发送心跳。

- MsgPropose 提议日志追加：上层可以通过调用 Step 函数并传递 MessageType_MsgPropose 消息，向当前的领导者提议追加数据到日志条目中。

- MsgAppend 日志追加请求：领导节点可以通过调用 Step 函数并传递 MessageType_MsgAppend 消息，向其他节点发送日志追加请求，用于进行日志的复制和同步。

- MsgRequestVote 请求选票：在选举过程中，候选人节点可以通过调用 Step 函数并传递 MessageType_MsgRequestVote 消息，向其他节点请求投票以获取支持。

- MsgSnapshot 快照请求：领导节点可以通过调用 Step 函数并传递 MessageType_MsgSnapshot 消息，向其他节点发送快照请求，用于进行快照的复制和应用。

- MsgHeartbeat 心跳请求：领导者节点可以通过调用 Step 函数并传递 MessageType_MsgHeartbeat 消息，向追随者发送心跳请求，用于维持领导者地位和通知追随者其仍然活跃。

- MsgTransferLeader 领导者转移请求：上层可以通过调用 Step 函数并传递 MessageType_MsgTransferLeader 消息，请求领导者进行领导权的转移。

- MsgTimeoutNow 领导者转移超时请求：领导者节点可以通过调用 Step 函数并传递 MessageType_MsgTimeoutNow 消息，通知领导权转移的目标节点立即超时并开始新的选举过程。

### become系列函数

- becomeFollower()：将节点转变为跟随者状态。
- becomeCandidate()：将节点转变为候选人状态。
- becomeLeader()：将节点转变为领导者状态。
  - 改变状态、更新heartbeatElapsed，electionElapsed。
  - 初始化Prs，更新Next、Match。
  - 添加一个在本地添加noop日志项的消息到 Step 函数中。

### 与上层交互的接口

Ready结构体

```go
type Ready struct {
	*SoftState
	pb.HardState
	Entries []pb.Entry
	Snapshot pb.Snapshot
	CommittedEntries []pb.Entry
	Messages []pb.Message
}
```

softState（软状态）是节点的当前易失性状态，它包含的信息相对较少且易变。softState 的主要目的是提供一些有关节点当前状态的快速访问，以便在需要时进行一些决策或信息展示。例如，softState 可以包含节点的角色（Follower、Candidate、Leader）以及与选举相关的信息，如当前任期号、投票候选人 ID 等。softState 是一个不需要持久化的状态，它可以在节点重启后被重新初始化。

hardState（硬状态）是节点的当前状态，它需要被持久化保存，以确保在节点故障或重新启动后能够恢复到之前的状态。hardState 包含的信息相对较多且相对稳定。它包含了节点的当前任期号、投票候选人 ID 和已提交的日志索引等关键信息。当节点状态发生变化时，如选举结果、日志追加或节点转变角色时，hardState 需要被更新和保存到稳定存储介质，以便在恢复节点时能够恢复到正确的状态。

softState 是节点的当前易失性状态，提供了节点状态的快速访问，但不需要持久化。hardState 是节点的当前状态，包含了关键的信息，需要被持久化保存以确保节点的持久性和一致性。

CommittedEntries 是已提交的日志条目到已持久化的日志条目，中间这部分是已持久化但未提交的日志条目。

- Ready()：返回一个 Ready 结构体，其中包含了当前节点的状态变化和需要发送给其他节点的消息。
- Advance()：将节点的状态推进到 Ready 中的 CommittedIndex 和 AppliedIndex。
- HasReady()：判断 Ready 中是否包含了需要发送给其他节点的消息。
  - 判断是否有消息需要发送
  - 判断是否有需要应用的条目
  - 判断是否有需要应用的快照
  - 判断是否有状态改变或持久化
  - 判断是否有需要持久化的条目

### 测试

![](./img/project2_2.png)

遇到的问题：
- 之前处理投票请求时忘了return，导致一直在投票没有停止
    ![](./img/project2_3.png)
- raft初始化后调用了becomeFollower，导致之前初始化的term, vote等信息被覆盖，从而出现一些奇怪的错误
- HasReady函数中，一开始将softState也加入到判断中，导致一直返回true，无法停止
- 之前没搞清楚什么时候重置选举超时时间，导致一些测试用例无法通过

## project2B

### 遇到的问题

1. SaveReadyState保存时忘记把db放进去了，导致一直报空指针
    ![](./img/project2_4.png)
2. process中处理请求时
    ![](./img/project2_5.png)
   
    在CmdType_Snap中加入
    
   p.cb.Txn = d.peerStorage.Engines.Kv.NewTransaction(false)
3. process函数中，在处理普通request时，要return返回的WriteBatch而不是传进去的wb
    ![](./img/project2_6.png)
    ![](./img/project2_7.png)