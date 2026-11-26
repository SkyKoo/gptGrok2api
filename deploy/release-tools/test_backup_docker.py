"""Opt-in backup rehearsal, using only existing local Docker images.

CFM_BACKUP_DOCKER=1 python3 -m unittest discover -s deploy/release-tools -p test_backup_docker.py
Creates two named temporary containers, no host ports/networks or production mounts.
"""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid

spec=importlib.util.spec_from_file_location('rolling_backup',Path(__file__).with_name('rolling-deploy.py'))
r=importlib.util.module_from_spec(spec);spec.loader.exec_module(r)

@unittest.skipUnless(os.environ.get('CFM_BACKUP_DOCKER')=='1','explicit isolated Docker rehearsal only')
class BackupDocker(unittest.TestCase):
    def test_database_row_guard_and_dump_restore_and_redis_snapshot(self):
        token=uuid.uuid4().hex[:12]; pg='cfm-backup-test-pg-'+token; redis='cfm-backup-test-redis-'+token
        host=r.Host(); created=[]
        with tempfile.TemporaryDirectory(prefix='cfm-backup-test-') as temporary:
            root=Path(temporary);(root/'data').mkdir();(root/'logs').mkdir();work=root/'backup';work.mkdir()
            (root/'data'/'business.json').write_text('{"fixture":"preserve"}')
            try:
                # Never pull, publish a port, mount host data or reuse a container.
                for name,image,args in [(pg,'postgres:17-alpine',['-e','POSTGRES_USER=fixture','-e','POSTGRES_DB=fixture','-e','POSTGRES_PASSWORD=fixture-local-only']),
                    (redis,'redis:7-alpine',[])]:
                    host.run(['docker','run','-d','--pull=never','--network=none','--name',name,'--label','io.cfm.rehearsal='+token,*args,image]);created.append(name)
                end=time.monotonic()+40
                while True:
                    try:
                        host.run(['docker','exec',pg,'pg_isready','-h','127.0.0.1','-U','fixture','-d','fixture']);break
                    except r.ReleaseError:
                        if time.monotonic()>end: raise
                        time.sleep(.2)
                tables=('mailbox_schema_migrations','mailboxes','mailbox_registration_blocks','mailbox_claims','mailbox_events','mailbox_registration_retirements','mailbox_sources')
                sql=''.join('CREATE TABLE '+table+' (id integer PRIMARY KEY,value text);INSERT INTO '+table+" VALUES(1,'fixture');" for table in tables)
                host.run(['docker','exec','-i',pg,'psql','-X','-v','ON_ERROR_STOP=1','-U','fixture','-d','fixture'],input=sql.encode())
                before=r.database_guard(host,pg)
                self.assertEqual(before,r.database_guard(host,pg))
                host.run(['docker','exec',redis,'redis-cli','SET','fixture','accepted'])
                snapshot=r.Snapshot(root,work,host);snapshot.sync(frozen=True);snapshot.database({'postgres':pg,'redis':redis});snapshot.finish()
                self.assertTrue((work/'complete').exists());self.assertTrue((work/'redis.rdb').read_bytes().startswith(b'REDIS'))
                host.run(['docker','exec',pg,'createdb','-U','fixture','restore_fixture'])
                host.run(['docker','exec','-i',pg,'pg_restore','-U','fixture','-d','restore_fixture'],input=(work/'postgres.dump').read_bytes())
                count=host.run(['docker','exec',pg,'psql','-X','-At','-U','fixture','-d','restore_fixture','-c','SELECT count(*) FROM mailboxes']).strip()
                self.assertEqual(count,b'1')
                host.run(['docker','exec',pg,'psql','-X','-v','ON_ERROR_STOP=1','-U','fixture','-d','fixture','-c',"UPDATE mailboxes SET value='changed' WHERE id=1"])
                self.assertNotEqual(before,r.database_guard(host,pg))
                for name in r.load(work/'checksums.json'):
                    self.assertEqual(r.file_digest(work/name),r.load(work/'checksums.json')[name])
            finally:
                for name in reversed(created):
                    subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,check=True)

if __name__=='__main__':unittest.main()
