// Local-only UI fixture. No database, real credentials or model calls.
import { createServer } from 'vite';
const seen = new Set();
const server = await createServer({ server: { host: '127.0.0.1', port: 5187, strictPort: true }, plugins: [{ name: 'public-ui-fixture', configureServer(server) {
  server.middlewares.use((req,res,next) => {
    if (!req.url?.startsWith('/api/public/agents/fixture')) return next();
    if (req.method === 'GET') {
      res.setHeader('Content-Type','application/json');
      res.end(JSON.stringify({code:0,data:{name:'MiniCoze 测试助手',description:'本地公开页面验收',openingMessage:'你好，请发送一条消息。',persistedRuns:true}})); return;
    }
    let body=''; req.on('data',chunk=>body+=chunk);
    req.on('end',()=>{
      const input=JSON.parse(body); const key=req.headers['idempotency-key']; const repeated=seen.has(key);seen.add(key);
      res.setHeader('Content-Type','text/event-stream');
      const send=(type,fields={})=>res.write(`event: ${type}\ndata: ${JSON.stringify({type,runId:'fixture-run',...fields})}\n\n`);
      send('run.created',{conversationId:input.conversationId||'fixture-conversation'});
      if(input.message==='断线'&&!repeated){send('message.delta',{content:'部分回答'});res.end();return;}
      const timer=setTimeout(()=>{send('message.delta',{content:`收到：${input.message}`});send('message.completed',{content:`收到：${input.message}`});send('run.completed');send('stream.done');res.end();},input.message==='等待'?5000:150);
      res.on('close',()=>clearTimeout(timer));
    });
  });
} }] });
await server.listen();
console.log('Public UI fixture: http://127.0.0.1:5187/share/agents/fixture');
