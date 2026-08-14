package bridge

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

type Page struct {
	Body  []byte
	Nonce string
	CSP   string
}

const PermissionsPolicy = "accelerometer=(), autoplay=(), camera=(), clipboard-read=(), clipboard-write=(), display-capture=(), encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), hid=(), idle-detection=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-create=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), usb=(), web-share=(), xr-spatial-tracking=()"

func Render(hostname, bootstrapToken string, batchBytes int) (Page, error) {
	if batchBytes <= 0 {
		return Page{}, errors.New("carrier batch size must be positive")
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return Page{}, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	hostJSON, _ := json.Marshal("https://" + hostname)
	tokenJSON, _ := json.Marshal(bootstrapToken)
	body := strings.ReplaceAll(document, "__NONCE__", nonce)
	body = strings.ReplaceAll(body, "__ORIGIN__", string(hostJSON))
	body = strings.ReplaceAll(body, "__BOOTSTRAP__", string(tokenJSON))
	body = strings.ReplaceAll(body, "__BATCH_LIMIT__", strconv.Itoa(batchBytes))
	if strings.Contains(body, "__NONCE__") || strings.Contains(body, "__ORIGIN__") || strings.Contains(body, "__BOOTSTRAP__") || strings.Contains(body, "__BATCH_LIMIT__") {
		return Page{}, errors.New("bridge template replacement failed")
	}
	return Page{
		Body:  []byte(body),
		Nonce: nonce,
		CSP: strings.Join([]string{
			"default-src 'none'",
			"base-uri 'none'",
			"child-src 'none'",
			"connect-src 'self'",
			"font-src 'none'",
			"form-action 'none'",
			"frame-ancestors http://127.0.0.1:*",
			"frame-src 'none'",
			"img-src 'none'",
			"manifest-src 'none'",
			"media-src 'none'",
			"object-src 'none'",
			"script-src 'nonce-" + nonce + "'",
			"style-src 'none'",
			"worker-src 'none'",
			"sandbox allow-same-origin allow-scripts",
		}, "; "),
	}, nil
}

const document = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Connection</title>
</head>
<body>
<script nonce="__NONCE__">
(()=>{
'use strict';
const relayOrigin=__ORIGIN__,bootstrap=__BOOTSTRAP__;
const fragment=location.hash,androidNonce=/^#android=([A-Za-z0-9_-]{43})$/.exec(fragment)?.[1]||'';
history.replaceState(null,'',location.pathname);
let initialized=false,closed=false,port=null,sessionToken='',upSequence=1,downCursor='0';
let createStarted=false,upRunning=false,pollController=null,queuedBytes=0,queuedItems=0;
const pending=[],upPending=[],queueLimit=33554432,queueItemLimit=16384,batchLimit=__BATCH_LIMIT__;
const status=state=>{if(port&&!closed)port.postMessage({t:'status',state})};
const pause=milliseconds=>new Promise(resolve=>setTimeout(resolve,milliseconds));
const options=(method,token,body,headers,signal,keepalive)=>({
 method,body,signal,keepalive:!!keepalive,mode:'same-origin',credentials:'omit',cache:'no-store',redirect:'error',referrerPolicy:'no-referrer',
 headers:Object.assign(token?{Authorization:'Bearer '+token}:{},body?{'Content-Type':'application/octet-stream'}:{},headers||{})
});
async function request(path,makeOptions){
 let delay=250;
 for(let attempt=0;attempt!==9;attempt++){
  const requestOptions=makeOptions(),controller=new AbortController();
  const external=requestOptions.signal,abort=()=>controller.abort();
  if(external)external.addEventListener('abort',abort,{once:true});
  requestOptions.signal=controller.signal;
  const timer=setTimeout(abort,90000);
  try{
   const response=await fetch(relayOrigin+path,requestOptions);
   if(response.status<500)return response;
   await response.arrayBuffer();
  }catch(error){if(closed||(external&&external.aborted))throw error}
  finally{clearTimeout(timer);if(external)external.removeEventListener('abort',abort)}
  status('reconnecting');
  await pause(delay+Math.floor(Math.random()*Math.max(1,delay/4)));
  delay=Math.min(delay*2,5000);
 }
 throw new Error('carrier retry limit reached');
}
function fail(){
 if(closed)return;
 status('failed');
 if(port)port.postMessage({t:'close'});
 close(false);
}
async function createSession(first){
 try{
  status('connecting');
  const response=await request('/api/v1/session',()=>options('POST',bootstrap,first));
  if(response.status!==200)throw new Error('session creation rejected');
  sessionToken=response.headers.get('X-Session-Token')||'';
  downCursor=response.headers.get('X-Down-Cursor')||'0';
  if(!sessionToken)throw new Error('missing session token');
  const welcome=await response.arrayBuffer();
  if(closed)return;
  port.postMessage(welcome,[welcome]);
  status('connected');
  for(const data of pending.splice(0))queueUp(data,true);
  poll();
 }catch(error){fail()}
}
function queueUp(data,counted){
 if(!counted){
  if(!data.byteLength||queuedBytes>queueLimit-data.byteLength||queuedItems>=queueItemLimit){fail();return}
  queuedBytes+=data.byteLength;queuedItems++;
 }
 upPending.push(data);
 runUp();
}
async function runUp(){
 if(upRunning)return;
 upRunning=true;
 try{
  while(!closed&&sessionToken&&upPending.length){
   let total=0,count=0;
   while(count<upPending.length&&(count===0||total+upPending[count].byteLength<=batchLimit)){
    total+=upPending[count].byteLength;count++;
   }
   const joined=new Uint8Array(total);let offset=0;
   for(const data of upPending.splice(0,count)){joined.set(new Uint8Array(data),offset);offset+=data.byteLength}
   const body=joined.buffer,sequence=String(upSequence);
   const response=await request('/api/v1/up',()=>options('POST',sessionToken,body,{'X-Up-Seq':sequence}));
   if(response.status!==204||response.headers.get('X-Up-Ack')!==sequence)throw new Error('uplink rejected');
   queuedBytes-=total;queuedItems-=count;
   port.postMessage({t:'traffic',up:total,down:0});
   upSequence++;
  }
 }catch(error){fail()}
 finally{upRunning=false;if(!closed&&sessionToken&&upPending.length)runUp()}
}
async function poll(){
 while(!closed&&sessionToken){
  try{
   pollController=new AbortController();
   const response=await request('/api/v1/down',()=>options('POST',sessionToken,null,{'X-Down-Cursor':downCursor},pollController.signal));
   if(response.status===204){status('connected');continue}
   if(response.status!==200)throw new Error('downlink rejected');
   const next=response.headers.get('X-Down-Cursor')||'';
   const data=await response.arrayBuffer();
   if(!next||!data.byteLength)throw new Error('invalid downlink response');
   if(closed)return;
   port.postMessage({t:'traffic',up:0,down:data.byteLength});
   port.postMessage(data,[data]);
   downCursor=next;
   status('connected');
  }catch(error){if(!closed)fail();return}
 }
}
function close(notifyServer){
 if(closed)return;
 closed=true;
 if(pollController)pollController.abort();
 if(notifyServer&&sessionToken){
  fetch(relayOrigin+'/api/v1/session',options('DELETE',sessionToken,null,null,undefined,true)).catch(()=>{});
 }
 if(port)port.close();
}
function activatePort(nextPort){
 initialized=true;
 port=nextPort;
 port.onmessage=message=>{
  if(message.data instanceof ArrayBuffer){
   if(!createStarted){createStarted=true;createSession(message.data)}
   else if(!sessionToken){
    if(!message.data.byteLength||queuedBytes>queueLimit-message.data.byteLength||queuedItems>=queueItemLimit){fail();return}
    queuedBytes+=message.data.byteLength;queuedItems++;pending.push(message.data);
   }
   else queueUp(message.data);
  }else if(message.data&&message.data.t==='close')close(true);
 };
 port.start();
 status('connecting');
}
addEventListener('message',event=>{
 if(initialized||event.source!==parent||event.data===null||typeof event.data!=='object')return;
 const keys=Object.keys(event.data).sort();
 if(keys.length!==2||keys[0]!=='t'||keys[1]!=='v'||event.data.t!=='tproxy-init'||event.data.v!==1||event.ports.length!==1)return;
 let source;
 try{source=new URL(event.origin)}catch(error){return}
 if(source.protocol!=='http:'||source.hostname!=='127.0.0.1'||!source.port||source.origin!==event.origin)return;
 activatePort(event.ports[0]);
},{once:false});
const androidBridge=globalThis.TelegramWebProxy;
if(!initialized&&androidNonce&&androidBridge&&typeof androidBridge.postMessage==='function'){
 const androidPort={onmessage:null,start(){},close(){androidBridge.onmessage=null},postMessage(value){
  if(value instanceof ArrayBuffer){
   const view=new DataView(value),frames=[];let offset=0;
   while(offset<value.byteLength){
    if(value.byteLength-offset<8||frames.length>=4096){fail();return}
    const size=view.getUint32(offset+4),end=offset+8+size;
    if(!size&&view.getUint8(offset)===2||size>1048576||end>value.byteLength){fail();return}
    frames.push([offset,end]);offset=end;
   }
   for(const frame of frames)androidBridge.postMessage(value.slice(frame[0],frame[1]));
  }else{
   androidBridge.postMessage(JSON.stringify(value));
  }
 }};
 androidBridge.onmessage=event=>{
  let data=event.data;
  if(typeof data==='string'){
   try{data=JSON.parse(data)}catch(error){return}
  }
  if(androidPort.onmessage)androidPort.onmessage({data});
 };
 activatePort(androidPort);
 androidBridge.postMessage(JSON.stringify({t:'tproxy-android-init',v:1,nonce:androidNonce}));
}
addEventListener('pagehide',()=>close(true),{once:true});
})();
</script>
</body>
</html>
`
