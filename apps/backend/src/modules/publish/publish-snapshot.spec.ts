import { PublishService } from './publish.service';

describe('published knowledge permissions', () => {
  it('copies binding identity and status without retaining mutable draft objects', () => {
    const service = new PublishService(null!, null!, null!, null!);
    const agent = {
      id: 'agent', workspaceId: 'workspace', name: 'demo',
      model: 'model', systemPrompt: 'frozen', temperature: 0.5,
      contextLimit: 10, pluginBindings: [], workflows: [],
      knowledgeBaseBindings: [
        { knowledgeBaseId: 'active', enabled: true, knowledgeBase: { name: 'Docs', status: 'ACTIVE' } },
        { knowledgeBaseId: 'disabled', enabled: false, knowledgeBase: { name: 'Hidden', status: 'ACTIVE' } },
        { knowledgeBaseId: 'archived', enabled: true, knowledgeBase: { name: 'Old', status: 'ARCHIVED' } },
      ],
    };
    const snapshot = service['buildAgentSnapshot'](agent as never);
    agent.knowledgeBaseBindings[0].knowledgeBase.name = 'Edited draft';
    agent.systemPrompt = 'Edited prompt';
    expect(snapshot.agent.systemPrompt).toBe('frozen');
    expect(snapshot.knowledge).toEqual([
      { knowledgeBaseId: 'active', name: 'Docs', enabled: true },
      { knowledgeBaseId: 'disabled', name: 'Hidden', enabled: false },
      { knowledgeBaseId: 'archived', name: 'Old', enabled: false },
    ]);
  });
});
