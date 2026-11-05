package message

// State Agent 状态机（移植自 apps/cli/docs/AGENT_LOOP.md detectAgentState）。
type State string

const (
	// StateNoTask 无任务（无消息）。
	StateNoTask State = "NO_TASK"
	// StateRunning 任务执行中。
	StateRunning State = "RUNNING"
	// StateStreaming API 流式接收中。
	StateStreaming State = "STREAMING"
	// StateWaitingForInput 等待用户应答（最后一条消息为非 partial 的 ask）。
	StateWaitingForInput State = "WAITING_FOR_INPUT"
	// StateIdle 任务完成或失败。
	StateIdle State = "IDLE"
	// StateResumable 任务暂停可恢复。
	StateResumable State = "RESUMABLE"
)

// DetectState 从消息序列推断 Agent 当前状态（移植 detectAgentState 逻辑）。
func DetectState(messages []ClineMessage) State {
	if len(messages) == 0 {
		return StateNoTask
	}

	last := messages[len(messages)-1]

	// 流式判定：partial 标记优先。
	if last.Partial {
		return StateStreaming
	}

	// api_req_started 且 cost 未回填 → 请求进行中。
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Type == TypeSay && m.Say == SayAPIReqStarted {
			if !m.Partial && m.Text != "" {
				// 已完结的 api_req_started 不再判定为 streaming，
				// 继续向前找（简化实现：找到即停）。
			}
			break
		}
	}

	// ask 判定：最后一条为非 partial ask。
	if last.Type == TypeAsk {
		switch last.Ask {
		case AskCompletionResult, AskAPIReqFailed, AskMistakeLimit, AskAutoApprovalMax:
			return StateIdle
		case AskResumeTask:
			return StateResumable
		default:
			return StateWaitingForInput
		}
	}

	return StateRunning
}
