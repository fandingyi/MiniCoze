// Local-only UI fixture: no credentials, database or model calls.
import { createServer } from 'vite';
const runs = new Map();
const attempts = new Set();
const server = await createServer({ server: { host: '127.0.0.1', port: 5187, strictPort: true }, plugins: [{ name: 'public-ui-fixture', configureServer(server) {
  server.middlewares.use((req,res,next) => {
    if (!req.url?.startsWith('/api/public/agents/fixture')) return next();
    const json=(status,data)=>{res.statusCode=status;res.setHeader('Content-Type','application/json');res.end(JSON.stringify({code:status===200?0:status,data}));};
    if(req.url.includes('/runs/') && req.headers['x-visitor-session']!=='fixture-session'){json(401,null);return;}
    if(req.method==='POST' && req.url.endsWith('/cancel')) {
      const id=req.url.split('/runs/')[1].replace(/\/cancel$/,'');
      const run=[...runs.values()].find(r=>r.id===id);
      if(run && run.status==='RUNNING')run.status='CANCELLED';
      json(run?200:404,run);return;
    }
    if (req.method === 'GET') {
      if(req.url.includes('/runs/')) {const id=req.url.split('/runs/')[1];const run=[...runs.values()].find(r=>r.id===id);json(run?200:404,run);return;}
      json(200,{name:'MiniCoze 测试助手',description:'本地公开页面验收',openingMessage:'你好，请发送一条消息。',persistedRuns:true,sessionId:'fixture-session'});return;
    }
    let body='';req.on('data',chunk=>body+=chunk);
    req.on('end',async()=>{
      const input=JSON.parse(body),key=req.headers['idempotency-key'],repeated=attempts.has(key);attempts.add(key);
      if(req.headers['x-visitor-session']!=='fixture-session'||input.message==='身份失效'){json(401,null);return;}
      if(input.message==='暂忙'&&!repeated){res.setHeader('Retry-After','2');json(429,null);return;}
      let run=runs.get(key);
      if(!run){run={id:`fixture-${runs.size+1}`,conversationId:input.conversationId||`conversation-${runs.size+1}`,status:'RUNNING',output:`收到：${input.message}`};runs.set(key,run);}
      else if(run.status==='RUNNING'){json(409,run);return;}
      if(input.message==='响应头延迟'&&!repeated) {
        // Persist a completed result before headers: an interrupted caller must replay the same key.
        run.status='COMPLETED';
        await new Promise(resolve=>setTimeout(resolve,3000));
        if(res.destroyed)return;
      }
      res.setHeader('X-Run-Id',run.id);res.setHeader('Content-Type','text/event-stream');
      const send=(type,fields={})=>res.write(`event: ${type}\ndata: ${JSON.stringify({type,runId:run.id,...fields})}\n\n`);
      send('run.created',{conversationId:run.conversationId});
      if(input.message==='断线'&&!repeated){run.status='COMPLETED';send('message.delta',{content:'部分回答'});res.end();return;}
      if(['处理中','取消测试'].includes(input.message)&&!repeated){send('message.delta',{content:'部分回答'});res.end();setTimeout(()=>{if(run.status==='RUNNING')run.status='COMPLETED';},input.message==='取消测试'?30000:3000);return;}
      const timer=setTimeout(()=>{run.status='COMPLETED';send('message.delta',{content:run.output});send('message.completed',{content:run.output});send('run.completed');send('stream.done');res.end();},input.message==='等待'?5000:150);
      res.on('close',()=>{clearTimeout(timer);if(run.status==='RUNNING')run.status='CANCELLED';});
    });
  });
} }] });
await server.listen();
console.log('Public UI fixture: http://127.0.0.1:5187/share/agents/fixture');
