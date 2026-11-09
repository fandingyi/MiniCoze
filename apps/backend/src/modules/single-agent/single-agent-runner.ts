import { Injectable } from '@nestjs/common';
import { randomUUID } from 'node:crypto';
import { RuntimeEvent, ToolCall } from '../../shared/types/agent';
import type {
  AgentExecutionInput,
  AgentExecutionStrategy,
} from '../../shared/types/runtime';
import { AiGatewayService } from '../ai-gateway/ai-gateway.service';

const MAX_AGENT_ROUNDS = 5;
const MAX_TOOL_CALLS_PER_RUN = 16;

@Injectable()
export class SingleAgentRunner implements AgentExecutionStrategy {
  readonly mode = 'single_agent' as const;

  constructor(private readonly aiGateway: AiGatewayService) {}

  async *stream({
    context,
    messages,
    toolExecutor,
  }: AgentExecutionInput): AsyncGenerator<RuntimeEvent, string, void> {
    const tools = context.agentConfig.tools ?? [];

    let executedToolCalls = 0;
    for (let round = 1; round <= MAX_AGENT_ROUNDS; round += 1) {
      const assistantMessageId = randomUUID();
      const stream = this.aiGateway.chatStream({
        messages,
        model: context.agentConfig.model,
        temperature: context.agentConfig.temperature,
        maxTokens: context.agentConfig.maxTokens,
        tools,
      });

      let toolCalls: ToolCall[] = [];
      const collected: string[] = [];

      for await (const chunk of stream) {
        if (chunk.content) {
          collected.push(chunk.content);
          yield {
            type: 'message.delta',
            runId: context.runId,
            messageId: assistantMessageId,
            content: chunk.content,
          };
        }

        if (chunk.toolCalls?.length) {
          toolCalls = chunk.toolCalls;
        }
      }

      const finalContent = collected.join('');

      if (!toolCalls.length) {
        yield {
          type: 'message.completed',
          runId: context.runId,
          messageId: assistantMessageId,
          content: finalContent,
        };
        return finalContent;
      }

      if (round === MAX_AGENT_ROUNDS) {
        throw new Error('AGENT_MAX_ROUNDS_EXCEEDED');
      }
      if (executedToolCalls + toolCalls.length > MAX_TOOL_CALLS_PER_RUN) {
        throw new Error('AGENT_TOOL_CALL_LIMIT_EXCEEDED');
      }

      messages.push({
        role: 'assistant',
        content: finalContent || null,
        tool_calls: toolCalls,
      });

      for (const toolCall of toolCalls) {
        executedToolCalls += 1;
        const parsedArgs = this.maskPreviewArgs(
          this.safeParse(toolCall.function.arguments),
        );
        const initialMetadata = this.parseFunctionName(toolCall.function.name);
        yield {
          type: 'tool.call.created',
          runId: context.runId,
          toolCallId: toolCall.id,
          name: toolCall.function.name,
          ...initialMetadata,
          args: parsedArgs,
        };

        try {
          const result = await toolExecutor.execute({
            toolCall,
            context,
          });
          yield {
            type: 'tool.call.completed',
            runId: context.runId,
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            ...initialMetadata,
            ...result.metadata,
            result: result.maskedOutput ?? result.output,
          };

          messages.push({
            role: 'tool',
            content: result.output,
            tool_call_id: toolCall.id,
            name: toolCall.function.name,
          });
        } catch (error) {
          const message =
            error instanceof Error ? error.message : String(error);
          yield {
            type: 'tool.call.failed',
            runId: context.runId,
            toolCallId: toolCall.id,
            name: toolCall.function.name,
            ...initialMetadata,
            error: message,
          };
          throw error;
        }
      }
    }

    throw new Error('AGENT_MAX_ROUNDS_EXCEEDED');
  }

  private safeParse(value: string): unknown {
    try {
      return JSON.parse(value) as unknown;
    } catch {
      return value;
    }
  }

  private parseFunctionName(name: string): {
    pluginCode?: string;
    toolCode?: string;
  } {
    const [pluginCode, toolCode] = name.split('__');
    return {
      pluginCode,
      toolCode,
    };
  }

  private maskPreviewArgs(value: unknown): unknown {
    if (!value || typeof value !== 'object' || Array.isArray(value)) {
      return value;
    }

    const sensitiveKeys = new Set([
      'token',
      'apiKey',
      'authorization',
      'password',
      'secret',
      'cookie',
    ]);

    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>).map(([key, val]) => [
        key,
        sensitiveKeys.has(key) ? '***' : val,
      ]),
    );
  }
}
