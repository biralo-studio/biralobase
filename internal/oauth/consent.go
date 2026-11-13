package oauth

import "html/template"

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize · Biralobase</title>
<style nonce="{{.Nonce}}">body{font:16px system-ui;background:#f5f5f2;color:#17201d;max-width:560px;margin:5vh auto;padding:24px}main{background:white;padding:28px;border-radius:16px}h1{font-size:24px}label{display:block;margin:16px 0 6px}input{box-sizing:border-box;width:100%;padding:12px;font:inherit}button{padding:12px 18px;margin:14px 8px 0 0;cursor:pointer}small{overflow-wrap:anywhere}#error{color:#a31818}#consent{display:none}.brand{display:flex;align-items:center;gap:12px;font-size:20px;font-weight:700;letter-spacing:-.03em}.brand svg{width:44px;height:44px;flex-shrink:0}</style>
<main><div class="brand"><svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64" fill="none" role="img" aria-labelledby="title">
  <title id="title">Biralobase black cat</title>
  <rect x="1" y="1" width="62" height="62" rx="16" fill="#faf8f2" stroke="#dedbd3"/>
  <path fill="#111116" d="M12 29 10 11Q10 7 14 10L25 18Q32 16 39 18L50 10Q54 7 54 11L52 29C58 45 47 56 32 56S6 45 12 29Z"/>
  <path fill="#34343d" d="m15 15 2 12 6-6Zm34 0-8 6 6 6Z"/>
  <ellipse cx="23" cy="34" rx="6" ry="4.5" fill="#e7c76b"/>
  <ellipse cx="41" cy="34" rx="6" ry="4.5" fill="#e7c76b"/>
  <ellipse cx="24" cy="34" rx="1.5" ry="3.5" fill="#111116"/>
  <ellipse cx="40" cy="34" rx="1.5" ry="3.5" fill="#111116"/>
  <path fill="#e7c76b" d="M29 43h6l-3 3Z"/>
</svg><span>Biralobase</span></div><h1>Connect {{.Name}} to your account</h1><p>This client requests the following permissions:</p><ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<p><small>Client: {{.Client}}<br>Return address: {{.Redirect}}</small></p><p>Only authorize a client you trust. Your existing collection access rules still apply.</p>
<form id="login"><label for="identity">Email</label><input id="identity" autocomplete="username" required><label for="password">Password</label><input id="password" type="password" autocomplete="current-password" required><button>Sign in</button></form>
<div id="consent"><p id="account"></p><button id="allow">Authorize</button><button id="deny">Deny</button></div><p id="error" role="alert"></p></main>
<script nonce="{{.Nonce}}">
const id={{.ID}}, collection={{.Collection}}; let token='';
const error=document.querySelector('#error');
const signedIn=record=>{document.querySelector('#login').style.display='none';document.querySelector('#consent').style.display='block';document.querySelector('#account').textContent='Signed in as '+(record.email||record.id)};
try{const saved=JSON.parse(localStorage.getItem('pocketbase_auth')||'null');if(saved?.token&&saved?.record?.collectionId===collection){token=saved.token;signedIn(saved.record)}}catch{}
document.querySelector('#login').addEventListener('submit',async event=>{event.preventDefault();error.textContent='';try{const result=await fetch('/api/collections/'+collection+'/auth-with-password',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({identity:document.querySelector('#identity').value,password:document.querySelector('#password').value})});const body=await result.json();if(!result.ok)throw Error(body.message||'Sign-in failed. If your account requires another sign-in method, sign in to your app first and reopen this page.');token=body.token;document.querySelector('#password').value='';signedIn(body.record)}catch(e){error.textContent=e.message}});
async function decide(allow){error.textContent='';try{const result=await fetch('/oauth/consent',{method:'POST',headers:{'Content-Type':'application/json','Authorization':token},body:JSON.stringify({id,allow})});const body=await result.json();if(!result.ok)throw Error(body.message||'Authorization failed');location.assign(body.redirect)}catch(e){error.textContent=e.message}}
document.querySelector('#allow').onclick=()=>decide(true);document.querySelector('#deny').onclick=()=>decide(false);
</script></html>`))
