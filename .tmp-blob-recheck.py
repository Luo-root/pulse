import subprocess, tempfile, os, sys

dirty = []
files = subprocess.run(['git', 'ls-files', '*.go'], capture_output=True, text=True).stdout.split()
for p in files:
    blob = subprocess.run(['git', 'show', f'HEAD:{p}'], capture_output=True).stdout
    if blob.startswith(b'\xef\xbb\xbf'):
        dirty.append((p, 'BOM'))
        continue
    with tempfile.NamedTemporaryFile(suffix='.go', delete=False) as f:
        f.write(blob)
        tmp = f.name
    r = subprocess.run(['gofmt', '-l', tmp], capture_output=True, text=True)
    os.unlink(tmp)
    if r.stdout.strip():
        dirty.append((p, 'gofmt'))
print(f'checked {len(files)} blobs at HEAD')
print('DIRTY:', dirty if dirty else 'NONE - ALL BLOBS CLEAN')
sys.exit(1 if dirty else 0)
