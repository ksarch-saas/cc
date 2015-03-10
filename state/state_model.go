package state

import (
	"log"

	"github.com/jxwr/cc/fsm"
	"github.com/jxwr/cc/meta"
	"github.com/jxwr/cc/redis"
)

const (
	StateRunning           = "RUNNING"
	StateWaitFailoverBegin = "WAIT_FAILOVER_BEGIN"
	StateWaitFailoverEnd   = "WAIT_FAILOVER_END"
	StateOffline           = "OFFLINE"
)

var (
	RunningState = &fsm.State{
		Name: StateRunning,
		OnEnter: func(ctx interface{}) {
			log.Println("Enter RUNNING state")
		},
		OnLeave: func(ctx interface{}) {
			log.Println("Leave RUNNING state")
		},
	}

	WaitFailoverBeginState = &fsm.State{
		Name: StateWaitFailoverBegin,
		OnEnter: func(ctx interface{}) {
			log.Println("Enter WAIT_FAILOVER_BEGIN state")
		},
		OnLeave: func(ctx interface{}) {
			log.Println("Leave WAIT_FAILOVER_BEGIN state")
		},
	}

	WaitFailoverEndState = &fsm.State{
		Name: StateWaitFailoverEnd,
		OnEnter: func(ctx interface{}) {
			log.Println("Enter WAIT_FAILOVER_END state")
		},
		OnLeave: func(ctx interface{}) {
			log.Println("Leave WAIT_FAILOVER_END state")
		},
	}

	OfflineState = &fsm.State{
		Name: StateOffline,
		OnEnter: func(ctx interface{}) {
			log.Println("Enter OFFLINE state")
		},
		OnLeave: func(ctx interface{}) {
			log.Println("Leave OFFLINE state")
		},
	}
)

/// Constraints

var (
	SlaveAutoFailoverConstraint = func(i interface{}) bool {
		ctx := i.(StateContext)
		cs := ctx.ClusterState
		ns := ctx.NodeState

		rs := cs.FindReplicaSetByNode(ns.Id())
		if rs == nil {
			return false
		}
		// 至少还有一个节点
		localRegionNodes := rs.RegionNodes(ns.node.Region)
		if len(localRegionNodes) < 2 {
			return false
		}
		// 最多一个故障节点(FAIL或不处于Running状态)
		for _, node := range localRegionNodes {
			if node.Id == ns.Id() {
				continue
			}
			nodeState := cs.FindNodeState(node.Id)
			if node.Fail || nodeState.CurrentState() != StateRunning {
				return false
			}
		}
		log.Println("can failover slave")
		return true
	}

	MasterAutoFailoverConstraint = func(i interface{}) bool {
		ctx := i.(StateContext)
		cs := ctx.ClusterState
		ns := ctx.NodeState

		// 如果AutoFailover没开，且不是执行Failover的信号
		if !meta.AutoFailover() && ctx.Input.Command != CMD_FAILOVER_BEGIN_SIGNAL {
			return false
		}

		rs := cs.FindReplicaSetByNode(ns.Id())
		if rs == nil {
			return false
		}
		// Region至少还有一个节点
		localRegionNodes := rs.RegionNodes(ns.node.Region)
		if len(localRegionNodes) < 2 {
			return false
		}
		// 最多一个故障节点(FAIL或不处于Running状态)
		for _, node := range localRegionNodes {
			if node.Id == ns.Id() {
				continue
			}
			nodeState := cs.FindNodeState(node.Id)
			if node.Fail || nodeState.CurrentState() != StateRunning {
				return false
			}
		}
		log.Println("Can do failover for master")
		return true
	}

	SlaveFailoverHandler = func(i interface{}) {
		ctx := i.(StateContext)
		cs := ctx.ClusterState
		ns := ctx.NodeState

		for _, n := range cs.AllNodeStates() {
			resp, err := redis.DisableRead(n.Addr(), ns.Id())
			if err == nil {
				log.Println("Disable read of slave:", resp, ns.Id())
				break
			}
		}
	}

	MasterFailoverHandler = func(i interface{}) {
		ctx := i.(StateContext)
		cs := ctx.ClusterState
		ns := ctx.NodeState
		masterId, err := cs.MaxReploffSlibing(ns.Id(), true)
		if err != nil {
			log.Printf("No slave can be used for failover %s\n", ns.Id())
			// 放到另一个线程做，避免死锁
			go ns.AdvanceFSM(cs, CMD_FAILOVER_END_SIGNAL)
		} else {
			go cs.RunFailoverTask(ns.Id(), masterId)
		}
	}
)

var (
	RedisNodeStateModel = fsm.NewStateModel()
)

func init() {
	RedisNodeStateModel.AddState(RunningState)
	RedisNodeStateModel.AddState(WaitFailoverBeginState)
	RedisNodeStateModel.AddState(WaitFailoverEndState)
	RedisNodeStateModel.AddState(OfflineState)

	/// State: (WaitFailoverRunning)

	// (a0) Running封禁了，进入Offline状态
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateRunning,
		To:         StateOffline,
		Input:      Input{F, F, ANY, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (a1) 节点挂了，且未封禁
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateRunning,
		To:         StateWaitFailoverBegin,
		Input:      Input{T, ANY, FAIL, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (a2) 节点挂了，且未封禁
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateRunning,
		To:         StateWaitFailoverBegin,
		Input:      Input{ANY, T, FAIL, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (a3) 节点挂了，从，且未封禁，且可以自动进行Failover
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateRunning,
		To:         StateWaitFailoverEnd,
		Input:      Input{T, ANY, FAIL, S, ANY},
		Priority:   1,
		Constraint: SlaveAutoFailoverConstraint,
		Apply:      SlaveFailoverHandler,
	})

	// (a4) 节点挂了，主，未封禁，且可以自动进行Failover
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateRunning,
		To:         StateWaitFailoverEnd,
		Input:      Input{T, T, FAIL, M, ANY},
		Priority:   1,
		Constraint: MasterAutoFailoverConstraint,
		Apply:      MasterFailoverHandler,
	})

	/// State: (WaitFailoverBegin)

	// (b0) 节点恢复了
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverBegin,
		To:         StateRunning,
		Input:      Input{ANY, ANY, FINE, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (b1) 主节点，Autofailover或手动继续执行Failover
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverBegin,
		To:         StateWaitFailoverEnd,
		Input:      Input{ANY, ANY, FAIL, M, ANY},
		Priority:   0,
		Constraint: MasterAutoFailoverConstraint,
		Apply:      MasterFailoverHandler,
	})

	// (b2) 从节点，AutoFailover或手动继续执行Failover
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverBegin,
		To:         StateWaitFailoverEnd,
		Input:      Input{ANY, ANY, FAIL, S, ANY},
		Priority:   0,
		Constraint: SlaveAutoFailoverConstraint,
		Apply:      SlaveFailoverHandler,
	})

	// (b3) 从节点，已经处于封禁状态，转到OFFLINE
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverBegin,
		To:         StateOffline,
		Input:      Input{F, F, FAIL, S, ANY},
		Priority:   1,
		Constraint: nil,
		Apply:      nil,
	})

	/// State: (WaitFailoverEnd)

	// (c0) 等待Failover执行结束信号
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverEnd,
		To:         StateOffline,
		Input:      Input{ANY, ANY, ANY, ANY, CMD_FAILOVER_END_SIGNAL},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (c1) 从挂了，且已经封禁
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateWaitFailoverEnd,
		To:         StateOffline,
		Input:      Input{F, F, FAIL, S, ANY},
		Priority:   1,
		Constraint: nil,
		Apply:      nil,
	})

	/// State: (Offline)

	// (d0) 节点恢复读标记
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateOffline,
		To:         StateRunning,
		Input:      Input{T, ANY, ANY, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (d1) 节点恢复写标记
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:       StateOffline,
		To:         StateRunning,
		Input:      Input{ANY, T, ANY, ANY, ANY},
		Priority:   0,
		Constraint: nil,
		Apply:      nil,
	})

	// (d2) 是主节，且挂了，需要进行Failover
	RedisNodeStateModel.AddTransition(&fsm.Transition{
		From:     StateOffline,
		To:       StateWaitFailoverBegin,
		Input:    Input{F, F, FAIL, M, ANY},
		Priority: 0,
		Constraint: func(i interface{}) bool {
			// Master故障，进行Failover之后，故障的节点仍然被标记为master。
			// 所以我们需要判断这个Master是否已经被处理过了。
			// 判断依据是节点处于FAIL状态，且没有slots
			ctx := i.(StateContext)
			ns := ctx.NodeState

			if ns.node.Fail && len(ns.node.Ranges) == 0 {
				return false
			}
			return true
		},
		Apply: nil,
	})
}

type StateContext struct {
	Input        Input
	ClusterState *ClusterState
	NodeState    *NodeState
}