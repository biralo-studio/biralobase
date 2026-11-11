import fs from 'node:fs';
import path from 'node:path';
const root=process.argv[2];
const file=path.join(root,'biralobase.schema.json');
const schema=JSON.parse(fs.readFileSync(file,'utf8'));
const users=schema.collections.find(c=>c.name==='users');
schema.collections.push({id:'sdknotes0000001',name:'sdk_notes',type:'base',fields:[
 {id:'sdktitle',name:'title',type:'text',required:true},
 {id:'sdkowner',name:'owner',type:'relation',collectionId:users.id,maxSelect:1,required:true},
 {id:'sdkfile',name:'attachment',type:'file',maxSelect:1},
],listRule:'owner = @request.auth.id',viewRule:'owner = @request.auth.id',createRule:"@request.auth.id != '' && @request.body.owner = @request.auth.id",updateRule:'owner = @request.auth.id',deleteRule:'owner = @request.auth.id'},
{id:'dartnotes000001',name:'dart_notes',type:'base',fields:[{id:'darttitle',name:'title',type:'text',required:true}],listRule:"@request.auth.id != ''",viewRule:"@request.auth.id != ''",createRule:"@request.auth.id != ''",updateRule:"@request.auth.id != ''",deleteRule:"@request.auth.id != ''"});
schema.settings={batch:{enabled:true}};
fs.writeFileSync(file,JSON.stringify(schema,null,2));
// This fixture exercises database/SDK behavior. Worker integration has its own
// suite, so it deliberately has no application functions to compile.
fs.rmSync(path.join(root,'functions'),{recursive:true});
