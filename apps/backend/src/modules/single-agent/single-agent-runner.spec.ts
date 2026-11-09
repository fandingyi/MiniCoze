import { SingleAgentRunner } from './single-agent-runner';
import type { AiGatewayService } from '../ai-gateway/ai-gateway.service';
import type { AgentExecutionInput } from '../../shared/types/runtime';

const toolCall = {
  id: 'call-1',
  type: 'function' as const,
  function: { name: 'system__echo', arguments: '{"text":"hello"}' },
};

function input(): AgentExecutionInput {
  return {
    context: {
      runId: 'run-1',
      conversationId: 'conversation-1',
      agentId: 'agent-1',
      userId: 'user-1',
      status: 'in_progress',
      input: { role: 'user', content: 'hello' },
      history: [],
      agentConfig: {
        id: 'agent-1',
        name: 'Agent',
        systemPrompt: 'help',
        model: 'test',
        temperature: 0,
        maxTokens: 100,
        contextLimit: 4,
        tools: [
          {
            type: 'function',
            function: {
              name: 'system__echo',
              description: 'echo',
              parameters: {},
            },
          },
        ],
      },
    },
    messages: [{ role: 'user', content: 'hello' }],
    toolExecutor: {
      execute: jest
        .fn()
        .mockResolvedValue({ toolCallId: 'call-1', output: 'hello' }),
    },
  };
}

async function* repeatedToolCall() {
  yield { toolCalls: [toolCall], isFinished: false };
}

describe('SingleAgentRunner', () => {
  it('stops a model that keeps requesting tools after five rounds', async () => {
    const gateway = {
      chatStream: jest.fn(() => repeatedToolCall()),
    } as unknown as AiGatewayService;
    const runner = new SingleAgentRunner(gateway);
    const execution = runner.stream(input());

    await expect(
      (async () => {
        for await (const _event of execution) {
          // Consume the emitted tool events until the strategy rejects.
        }
      })(),
    ).rejects.toThrow('AGENT_MAX_ROUNDS_EXCEEDED');

    expect(gateway.chatStream).toHaveBeenCalledTimes(5);
  });
});
