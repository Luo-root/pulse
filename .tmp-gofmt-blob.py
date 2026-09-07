import subprocess, tempfile, os

files = subprocess.run(['git','diff','main...HEAD','--name-only'], capture_output=True, text=True).stdout.split()
gofiles = [f for f in files if f.endswith('.go')]
tmpdir = tempfile.mkdtemp()
real, noise = [], []
for f in gofiles:
    blob = subprocess.run(['git','show','HEAD:'+f], capture_output=True).stdout
    crlf = b'\r\n' in blob
    bom = blob.startswith(b'\xef\xbb\xbf')
    p = os.path.join(tmpdir, os.path.basename(f))
    with open(p,'wb') as fh: fh.write(blob)
    r = subprocess.run(['gofmt','-l',p], capture_output=True, text=True)
    dirty = r.stdout.strip() != ''
    tag = 'REAL-DIRTY' if dirty else 'clean'
    if dirty: real.append(f)
    print(f"{tag:10} CRLF={crlf} BOM={bom} {f}")
print(f"\nreal gofmt issues: {len(real)}")
