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

func Render(hostname, bootstrapToken, carrierMode string, batchBytes int) (Page, error) {
	if batchBytes <= 0 {
		return Page{}, errors.New("carrier batch size must be positive")
	}
	if carrierMode != "https" && carrierMode != "https-lanes" && carrierMode != "websocket" {
		return Page{}, errors.New("invalid carrier mode")
	}
	nonceBytes := make([]byte, 18)
	if _, err := rand.Read(nonceBytes); err != nil {
		return Page{}, err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	hostJSON, _ := json.Marshal("https://" + hostname)
	tokenJSON, _ := json.Marshal(bootstrapToken)
	carrierJSON, _ := json.Marshal(carrierMode)
	body := strings.ReplaceAll(document, "__NONCE__", nonce)
	body = strings.ReplaceAll(body, "__ORIGIN__", string(hostJSON))
	body = strings.ReplaceAll(body, "__BOOTSTRAP__", string(tokenJSON))
	body = strings.ReplaceAll(body, "__CARRIER_MODE__", string(carrierJSON))
	body = strings.ReplaceAll(body, "__BATCH_LIMIT__", strconv.Itoa(batchBytes))
	if strings.Contains(body, "__NONCE__") || strings.Contains(body, "__ORIGIN__") || strings.Contains(body, "__BOOTSTRAP__") || strings.Contains(body, "__CARRIER_MODE__") || strings.Contains(body, "__BATCH_LIMIT__") {
		return Page{}, errors.New("bridge template replacement failed")
	}
	return Page{
		Body:  []byte(body),
		Nonce: nonce,
		CSP: strings.Join([]string{
			"default-src 'none'",
			"base-uri 'none'",
			"child-src 'none'",
			"connect-src 'self' wss://" + hostname,
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
const relayOrigin=__ORIGIN__,bootstrap=__BOOTSTRAP__,carrierMode=__CARRIER_MODE__;
const fragment=location.hash,androidNonce=/^#android=([A-Za-z0-9_-]{43})$/.exec(fragment)?.[1]||'';
history.replaceState(null,'',location.pathname);
let initialized=false,closed=false,port=null,sessionToken='',createStarted=false;
let queuedBytes=0,queuedItems=0,pollController=null,webSocket=null,webSocketTimer=0;
const pending=[],upPending=[],lanes=new Map(),closedLanes=new Set(),closedLaneOrder=[];
const queueLimit=33554432,queueItemLimit=16384,closedLaneLimit=4096;
const laneQueueLimit=8388608,laneItemLimit=1024,batchLimit=__BATCH_LIMIT__;
let upSequence=1,downCursor='0',upRunning=false;
const status=state=>{if(port&&!closed)port.postMessage({t:'status',state})};
const pause=milliseconds=>new Promise(resolve=>setTimeout(resolve,milliseconds));
const options=(method,token,body,headers,signal,keepalive)=>({
 method,body,signal,keepalive:!!keepalive,mode:'same-origin',credentials:'omit',cache:'no-store',redirect:'error',referrerPolicy:'no-referrer',
 headers:Object.assign(token?{Authorization:'Bearer '+token}:{},body?{'Content-Type':'application/octet-stream'}:{},headers||{})
});
const bufferedBytes=()=>webSocket&&webSocket.readyState===WebSocket.OPEN?webSocket.bufferedAmount:0;
function reserve(data,lane){
 if(!data.byteLength||queuedBytes+bufferedBytes()>queueLimit-data.byteLength||queuedItems>=queueItemLimit)return false;
 if(lane&&(lane.bytes>laneQueueLimit-data.byteLength||lane.items>=laneItemLimit))return false;
 queuedBytes+=data.byteLength;queuedItems++;
 if(lane){lane.bytes+=data.byteLength;lane.items++}
 return true;
}
function release(bytes,items,lane){
 queuedBytes-=bytes;queuedItems-=items;
 if(lane){lane.bytes-=bytes;lane.items-=items}
}
function splitFrames(value){
 const view=new DataView(value),result=[];let offset=0;
 while(offset<value.byteLength){
  if(value.byteLength-offset<8||result.length>=4096)throw new Error('invalid frame batch');
  const type=view.getUint8(offset),id=(view.getUint8(offset+1)<<16)|(view.getUint8(offset+2)<<8)|view.getUint8(offset+3);
  const size=view.getUint32(offset+4),end=offset+8+size;
  if((type===2&&!size)||size>1048576||end>value.byteLength)throw new Error('invalid frame');
  result.push({type,id,data:offset===0&&end===value.byteLength?value:value.slice(offset,end)});offset=end;
 }
 if(!result.length)throw new Error('empty frame batch');
 return result;
}
function joinPending(values){
 let total=0,count=0;
 while(count<values.length&&(count===0||total+values[count].byteLength<=batchLimit)){total+=values[count].byteLength;count++}
 const joined=new Uint8Array(total);let offset=0;
 for(const data of values.splice(0,count)){joined.set(new Uint8Array(data),offset);offset+=data.byteLength}
 return {body:joined.buffer,total,count};
}
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
  if(response.status!==200||response.headers.get('X-Carrier-Mode')!==carrierMode)throw new Error('session creation rejected');
  sessionToken=response.headers.get('X-Session-Token')||'';
  downCursor=response.headers.get('X-Down-Cursor')||'0';
  if(!sessionToken)throw new Error('missing session token');
  const welcome=await response.arrayBuffer();
  if(carrierMode==='websocket')await openWebSocket();
  if(closed)return;
  port.postMessage(welcome,[welcome]);
  status('connected');
  for(const data of pending.splice(0)){release(data.byteLength,1,null);queueCarrier(data)}
  if(carrierMode==='https')poll();
  else if(carrierMode==='https-lanes')pollLane(ensureLane(0));
 }catch(error){fail()}
}
function queueCarrier(data){
 try{
  if(carrierMode==='https')queueUp(data);
  else if(carrierMode==='https-lanes')for(const value of splitFrames(data))queueLane(value);
  else queueWebSocket(data);
 }catch(error){fail()}
}
function queueUp(data){
 if(!reserve(data,null)){fail();return}
 upPending.push(data);runUp();
}
async function runUp(){
 if(upRunning)return;
 upRunning=true;
 try{
  while(!closed&&sessionToken&&upPending.length){
   const batch=joinPending(upPending),sequence=String(upSequence);
   const response=await request('/api/v1/up',()=>options('POST',sessionToken,batch.body,{'X-Up-Seq':sequence}));
   if(response.status!==204||response.headers.get('X-Up-Ack')!==sequence)throw new Error('uplink rejected');
   release(batch.total,batch.count,null);port.postMessage({t:'traffic',up:batch.total,down:0});upSequence++;
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
   const next=response.headers.get('X-Down-Cursor')||'',data=await response.arrayBuffer();
   if(!next||!data.byteLength)throw new Error('invalid downlink response');
   if(closed)return;
   port.postMessage({t:'traffic',up:0,down:data.byteLength});port.postMessage(data,[data]);downCursor=next;status('connected');
  }catch(error){if(!closed)fail();return}
 }
}
function ensureLane(id){
 let lane=lanes.get(id);
 if(!lane){lane={id,sequence:1,cursor:'0',pending:[],bytes:0,items:0,running:false,polling:false};lanes.set(id,lane)}
 return lane;
}
function rememberLaneClosed(id){
 if(!id||closedLanes.has(id))return;
 if(closedLaneOrder.length===closedLaneLimit)closedLanes.delete(closedLaneOrder.shift());
 closedLanes.add(id);closedLaneOrder.push(id);
}
function queueLane(value){
 let lane=lanes.get(value.id);
 if(!lane&&closedLanes.has(value.id)){
  if(value.type===2||value.type===3||value.type===4)return;
  throw new Error('closed lane was reused');
 }
 if(!lane&&value.type!==1)throw new Error('lane did not begin with OPEN');
 lane=lane||ensureLane(value.id);
 if(!reserve(value.data,lane)){fail();return}
 lane.pending.push(value.data);runLaneUp(lane);
}
async function runLaneUp(lane){
 if(lane.running)return;
 lane.running=true;
 try{
  while(!closed&&sessionToken&&lane.pending.length){
   const batch=joinPending(lane.pending),sequence=String(lane.sequence),laneID=String(lane.id);
   const response=await request('/api/v1/up',()=>options('POST',sessionToken,batch.body,{'X-Up-Seq':sequence,'X-Lane-ID':laneID}));
   if(response.status!==204||response.headers.get('X-Up-Ack')!==sequence)throw new Error('lane uplink rejected');
   release(batch.total,batch.count,lane);port.postMessage({t:'traffic',up:batch.total,down:0});lane.sequence++;
   if(!lane.polling)pollLane(lane);
  }
 }catch(error){fail()}
 finally{lane.running=false;if(!closed&&sessionToken&&lane.pending.length)runLaneUp(lane)}
}
async function pollLane(lane){
 if(lane.polling)return;
 lane.polling=true;
 try{
  while(!closed&&sessionToken&&lanes.get(lane.id)===lane){
   const controller=new AbortController(),laneID=String(lane.id);
   lane.controller=controller;
   const response=await request('/api/v1/down',()=>options('POST',sessionToken,null,{'X-Down-Cursor':lane.cursor,'X-Lane-ID':laneID},controller.signal));
   if(response.status===204){
    if(response.headers.get('X-Lane-Closed')==='1'){lanes.delete(lane.id);rememberLaneClosed(lane.id);return}
    status('connected');continue;
   }
   if(response.status!==200)throw new Error('lane downlink rejected');
   const next=response.headers.get('X-Down-Cursor')||'',data=await response.arrayBuffer();
   if(!next||!data.byteLength)throw new Error('invalid lane downlink response');
   for(const value of splitFrames(data))if(value.id!==lane.id)throw new Error('cross-lane frame');
   if(closed)return;
   port.postMessage({t:'traffic',up:0,down:data.byteLength});port.postMessage(data,[data]);lane.cursor=next;status('connected');
  }
 }catch(error){if(!closed)fail()}
 finally{lane.polling=false;lane.controller=null}
}
function openWebSocket(){
 return new Promise((resolve,reject)=>{
  const target=relayOrigin.replace(/^https:/,'wss:')+'/api/v1/ws',socket=new WebSocket(target,'tproxy-v1.'+sessionToken);
  webSocket=socket;socket.binaryType='arraybuffer';
  socket.onopen=()=>resolve();
  socket.onmessage=event=>{
   if(!(event.data instanceof ArrayBuffer)||!event.data.byteLength){fail();return}
   port.postMessage({t:'traffic',up:0,down:event.data.byteLength});port.postMessage(event.data,[event.data]);status('connected');
  };
  socket.onerror=()=>reject(new Error('websocket failed'));
  socket.onclose=()=>{if(!closed)fail()};
 });
}
function queueWebSocket(data){
 if(!reserve(data,null)){fail();return}
 upPending.push(data);runWebSocketUp();
}
function runWebSocketUp(){
 if(closed||!webSocket||webSocket.readyState!==WebSocket.OPEN||!upPending.length)return;
 if(webSocket.bufferedAmount+queuedBytes>queueLimit||webSocket.bufferedAmount>=batchLimit){
  if(!webSocketTimer)webSocketTimer=setTimeout(()=>{webSocketTimer=0;runWebSocketUp()},10);
  return;
 }
 try{
  const batch=joinPending(upPending);webSocket.send(batch.body);release(batch.total,batch.count,null);
  port.postMessage({t:'traffic',up:batch.total,down:0});
  if(upPending.length)queueMicrotask(runWebSocketUp);
 }catch(error){fail()}
}
function close(notifyServer){
 if(closed)return;
 closed=true;
 if(pollController)pollController.abort();
 for(const lane of lanes.values())if(lane.controller)lane.controller.abort();
 if(webSocketTimer)clearTimeout(webSocketTimer);
 if(webSocket)webSocket.close();
 if(notifyServer&&sessionToken)fetch(relayOrigin+'/api/v1/session',options('DELETE',sessionToken,null,null,undefined,true)).catch(()=>{});
 if(port)port.close();
}
function activatePort(nextPort){
 initialized=true;port=nextPort;
 port.onmessage=message=>{
  if(message.data instanceof ArrayBuffer){
   if(!createStarted){createStarted=true;createSession(message.data)}
   else if(!sessionToken){if(!reserve(message.data,null)){fail();return}pending.push(message.data)}
   else queueCarrier(message.data);
  }else if(message.data&&message.data.t==='close')close(true);
 };
 port.start();status('connecting');
}
addEventListener('message',event=>{
 if(initialized||event.source!==parent||event.data===null||typeof event.data!=='object')return;
 const keys=Object.keys(event.data).sort();
 if(keys.length!==2||keys[0]!=='t'||keys[1]!=='v'||event.data.t!=='tproxy-init'||event.data.v!==1||event.ports.length!==1)return;
 let source;try{source=new URL(event.origin)}catch(error){return}
 if(source.protocol!=='http:'||source.hostname!=='127.0.0.1'||!source.port||source.origin!==event.origin)return;
 activatePort(event.ports[0]);
},{once:false});
const androidBridge=globalThis.TelegramWebProxy;
if(!initialized&&androidNonce&&androidBridge&&typeof androidBridge.postMessage==='function'){
 const androidPort={onmessage:null,start(){},close(){androidBridge.onmessage=null},postMessage(value){
  if(value instanceof ArrayBuffer){
   let frames;try{frames=splitFrames(value)}catch(error){fail();return}
   for(const frame of frames)androidBridge.postMessage(frame.data);
  }else androidBridge.postMessage(JSON.stringify(value));
 }};
 androidBridge.onmessage=event=>{
  let data=event.data;if(typeof data==='string'){try{data=JSON.parse(data)}catch(error){return}}
  if(androidPort.onmessage)androidPort.onmessage({data});
 };
 activatePort(androidPort);androidBridge.postMessage(JSON.stringify({t:'tproxy-android-init',v:1,nonce:androidNonce}));
}
addEventListener('pagehide',()=>close(true),{once:true});
})();
</script>
</body>
</html>
`
