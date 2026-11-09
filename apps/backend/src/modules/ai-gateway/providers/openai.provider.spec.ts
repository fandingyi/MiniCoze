import { BusinessException } from '../../../common/exceptions/business.exception';
import type { AiGenerateRequest, AiProviderConfig } from '../types';
import { AiProvider } from '../types';
import { OpenAiProvider } from './openai.provider';

const config: AiProviderConfig = {
  provider: AiProvider.OPENAI,
  apiKey: 'test-key',
  baseUrl: 'https://model.example/v1',
  defaultModel: 'test-model',
};

const request: AiGenerateRequest = {
  model: 'test-model',
  messages: [{ role: 'user', content: 'hello' }],
};

describe('OpenAiProvider stream safety', () => {
  let fetchMock: jest.SpyInstance;

  beforeEach(() => {
    fetchMock = jest.spyOn(global, 'fetch');
  });

  afterEach(() => {
    fetchMock.mockRestore();
  });

  it('cleans up the request when the provider returns a non-success response', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: { message: 'rate limited' } }), {
        status: 429,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const stream = new OpenAiProvider(config).generateStream(request);

    await expect(stream.next()).rejects.toBeInstanceOf(BusinessException);

    const signal = fetchMock.mock.calls[0][1].signal as AbortSignal;
    expect(signal.aborted).toBe(true);
  });

  it('rejects a stream that exceeds the unfinished-event buffer limit', async () => {
    const source = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(
          new TextEncoder().encode(`data: ${'x'.repeat(1024 * 1024)}`),
        );
      },
    });
    fetchMock.mockResolvedValueOnce(
      new Response(source, {
        status: 200,
        headers: { 'Content-Type': 'text/event-stream' },
      }),
    );
    const stream = new OpenAiProvider(config).generateStream(request);

    await expect(stream.next()).rejects.toBeInstanceOf(BusinessException);
    const signal = fetchMock.mock.calls[0][1].signal as AbortSignal;
    expect(signal.aborted).toBe(true);
  });
});
