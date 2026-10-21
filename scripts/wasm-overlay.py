#!/usr/bin/env python3
"""Apply the one WASM portability fix without modifying upstream or module caches."""
import json
import pathlib
import shutil
import subprocess

root = pathlib.Path(__file__).resolve().parents[1]
module = json.loads(subprocess.check_output(
    ['go', 'mod', 'download', '-json', 'github.com/Yiling-J/theine-go'], cwd=root / 'engine'))
source = pathlib.Path(module['Dir']) / 'internal/xruntime/xruntime.go'
text = source.read_text()
old = 'CacheLineSize = unsafe.Sizeof(cpu.CacheLinePad{})'
if text.count(old) != 1:
    raise SystemExit('theine-go changed: re-review the WASM cache-line portability patch')
out = root / 'dist'
out.mkdir(exist_ok=True)
copy = out / 'theine-go'
if copy.exists():
    for directory in [copy, *[p for p in copy.rglob("*") if p.is_dir()]]:
        directory.chmod(0o755)
    shutil.rmtree(copy)
shutil.copytree(module['Dir'], copy)
for directory in [copy, *[p for p in copy.rglob('*') if p.is_dir()]]:
    directory.chmod(0o755)
patched = copy / 'internal/xruntime/xruntime.go'
patched.chmod(0o644)
patched.write_text(text.replace(old, 'CacheLineSize = 64 + unsafe.Sizeof(cpu.CacheLinePad{})'))
(out / 'wasm.mod').write_text((root / 'engine/go.mod').read_text() +
    f'\nreplace github.com/Yiling-J/theine-go => {copy}\n')
shutil.copyfile(root / 'engine/go.sum', out / 'wasm.sum')
