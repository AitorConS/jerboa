#!/usr/bin/env python3
"""Record execution, queue and approval latency separately where GitHub exposes it."""
import datetime,json,os,subprocess

def timestamp(value):return datetime.datetime.fromisoformat(value.replace('Z','+00:00'))
repo=os.environ['GITHUB_REPOSITORY'];run=os.environ['GITHUB_RUN_ID']
pages=json.loads(subprocess.check_output(['gh','api','--paginate','--slurp',f'repos/{repo}/actions/runs/{run}/jobs?per_page=100']))
rows=[]
for page in pages:
 for j in page['jobs']:
  if not j.get('started_at') or not j.get('completed_at') or j['conclusion']=='skipped':continue
  seconds=max(0,(timestamp(j['completed_at'])-timestamp(j['started_at'])).total_seconds())
  rows.append({'name':j['name'],'result':j['conclusion'],'execution_seconds':seconds,'started_at':j['started_at'],'completed_at':j['completed_at']})
# GitHub's jobs response does not expose a reliable ready-for-runner timestamp;
# do not mislabel prerequisite/approval time as runner queue time.
with open('ci-timings.json','w') as f:json.dump({'run_id':run,'jobs':rows,'queue_seconds':None,'note':'Queue/approval need runner telemetry; not inferred from workflow creation'},f,indent=2)
with open(os.environ['GITHUB_STEP_SUMMARY'],'a') as f:
 f.write('## Execution timings\n\n| Job | Result | Minutes |\n|---|---|---:|\n')
 for r in rows:f.write(f"| {r['name']} | {r['result']} | {r['execution_seconds']/60:.1f} |\n")
