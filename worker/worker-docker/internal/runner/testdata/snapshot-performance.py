import hashlib
import json
import os
import sys

action, total, count = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
root = '/snapshot-performance'
if action == 'seed':
    os.mkdir(root, 0o700)
digest = hashlib.sha256()
for index in range(count):
    size = total // count + (index < total % count)
    name = f'{index:08d}'
    path = os.path.join(root, name)
    file_digest = hashlib.sha256()
    if action == 'seed':
        with open(path, 'xb') as output:
            remaining = size
            while remaining:
                chunk = os.urandom(min(remaining, 1024 * 1024))
                output.write(chunk)
                file_digest.update(chunk)
                remaining -= len(chunk)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(path, 0o640)
    elif action == 'verify':
        assert os.stat(path).st_size == size
        assert os.stat(path).st_mode & 0o777 == 0o640
        with open(path, 'rb') as source:
            while chunk := source.read(1024 * 1024):
                file_digest.update(chunk)
    else:
        raise ValueError('unknown action')
    digest.update(name.encode('ascii'))
    digest.update(file_digest.digest())
assert len(os.listdir(root)) == count
print(json.dumps({'bytes': total, 'files': count, 'sha256': digest.hexdigest()}))
