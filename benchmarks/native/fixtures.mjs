import { workloads, root } from "../workloads.mjs";
import { mkdir, writeFile } from "node:fs/promises";
const fixtures = (await workloads()).map(w => {
  const modelBody = JSON.stringify(w.model);
  const checkBody = JSON.stringify(w.check);
  const order = (a,b) => { for (const k of ["object","relation","user"]) { if(a[k]<b[k])return -1;if(a[k]>b[k])return 1; }return 0; };
  const tupleBodies = [...w.tuples].sort(order).map(t => JSON.stringify(t));
  const request = { operation:"check",store_id:"01ARZ3NDEKTSV4RRFFQ69G5FAV",model_id:w.model.id,check:w.check };
  return {...w,modelBody,checkBody,tupleBodies,hydratedInput:JSON.stringify({...request,results:[[[modelBody]],tupleBodies.map(b=>[b])]})};
});
await mkdir(`${root}dist/native`,{recursive:true});
await writeFile(`${root}dist/native/fixtures.json`,JSON.stringify(fixtures));
