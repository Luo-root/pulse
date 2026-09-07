import subprocess, tempfile, os
files = subprocess.run(['git','diff','main...HEAD','--name-only'], capture_output=True, text=True).stdout.split()
gofiles = [f for f in files if f.endswith('.go')]
tmpdir = tempfile.mkdtemp()
real = []
for f in gofiles:
    blob = subprocess.run(['git','show','HEAD:'+f], capture_output=True).stdout
    if blob.startswith(b'fatal'): continue
    p = os.path.join(tmpdir, 'x.go')
    with open(p,'wb') as fh: fh.write(blob)
    d = subprocess.run(['gofmt','-d',p], capture_output=True).stdout.decode('utf-8','replace')
    lines = [l for l in d.splitlines() if l.startswith(('+','-')) and not l.startswith(('+++','---'))]
    if lines:
        real.append((f, len(lines), blob.startswith(b'\xef\xbb\xbf')))
for f, n, bom in real: print(f"DIRTY {n:>3} changes BOM={bom} {f}")
print(f"total dirty: {len(real)}")
