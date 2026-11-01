import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'
import { parse } from '@vue/compiler-sfc'

// Exercise the actual SFC handlers, isolated from DOM-only setup.
function statement(path, name) {
 const {descriptor} = parse(fs.readFileSync(new URL(path, import.meta.url), 'utf8'))
 const ast = ts.createSourceFile('component.ts', descriptor.scriptSetup.content, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS)
 const found = ast.statements.find(n => ts.isFunctionDeclaration(n) ? n.name?.text === name : ts.isVariableStatement(n) && n.declarationList.declarations.some(d => d.name.getText(ast) === name))
 assert.ok(found, `missing handler ${name}`)
 return ts.transpileModule(found.getText(ast), {compilerOptions:{target:ts.ScriptTarget.ES2022}}).outputText
}
test('selecting video emits the mode update and closes menus', () => {
 const events=[];let closed=0
 const sandbox={props:{mode:'image'},computed:x=>x,closeFloatingMenus:()=>closed++,emit:(...args)=>events.push(args)}
 vm.runInNewContext(statement('../src/components/studio/StudioComposer.vue','modeValue')+';modeValue.set("video");modeValue.set(["video"]);modeValue.set("bogus")',sandbox)
 assert.deepEqual(events,[['update:mode','video'],['update:mode','video']])
 assert.equal(closed,2)
})
test('adding video reference images preserves video mode', async () => {
 for (const mode of ['chat','video','image','search']) {
  const composeMode={value:mode}
  const sandbox={composeMode,referenceRuntime:{append:async()=>true},composerRuntime:{activateImageMode:()=>{composeMode.value='image'}}}
  await vm.runInNewContext(statement('../src/views/Studio.vue','appendFiles')+';appendFiles([])',sandbox)
  assert.equal(composeMode.value,mode==='search'?'image':mode)
 }
})
