#!/usr/bin/env bash
# Ephemeral Windows runner for the candidate's installer smoke (release.yml).
#
# Each candidate run starts one EC2 Windows instance from the launch template and
# image prepared by the jerboa-infra AWS setup, registers it as a just-in-time
# (single-job) runner with a label unique to the run, and terminates it afterwards.
#
# Usage (from release.yml, GitHub-hosted Linux runner):
#   windows-runner.sh check       require the windows-runner environment's settings
#   windows-runner.sh launch      register the JIT runner, start the instance, wait
#                                 for the runner to come online
#   windows-runner.sh terminate   terminate INSTANCE_ID
#   windows-runner.sh deregister  remove RUNNER_NAME's registration if it remains
#
# Environment: check reads the WINDOWS_RUNNER_* variables and HAS_APP_KEY;
# the others take ROLE_ARN, AWS_REGION, LAUNCH_TEMPLATE_ID (AWS); GH_TOKEN, a GitHub
# App installation token allowed to manage this repository's runners; RUNNER_NAME
# and RUNNER_LABEL; INSTANCE_ID for terminate. Tokens, credentials and the runner
# configuration are masked and never printed.
set -euo pipefail

repo="${GITHUB_REPOSITORY:?}"
run_tag="${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"

err() { echo "::error title=Windows runner::$*" >&2; exit 1; }

# assume_role: exchange this job's OIDC token for short-lived credentials of the
# role that may only launch and terminate the runner instances.
assume_role() {
  [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] || err "no OIDC token available: the job needs 'id-token: write'"
  local oidc creds
  oidc=$(curl -fsS -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
    "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=sts.amazonaws.com" | jq -r .value)
  if [ -z "$oidc" ] || [ "$oidc" = null ]; then err "could not obtain an OIDC token"; fi
  echo "::add-mask::$oidc"
  creds=$(aws sts assume-role-with-web-identity --region "$AWS_REGION" \
    --role-arn "$ROLE_ARN" --role-session-name "jerboa-windows-runner-${run_tag}" \
    --web-identity-token "$oidc" --duration-seconds 900 \
    --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text)
  read -r AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN <<<"$creds"
  echo "::add-mask::$AWS_ACCESS_KEY_ID"
  echo "::add-mask::$AWS_SECRET_ACCESS_KEY"
  echo "::add-mask::$AWS_SESSION_TOKEN"
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
  unset oidc creds
}

cmd_check() {
  local missing=() v
  for v in ROLE_ARN REGION LAUNCH_TEMPLATE_ID APP_ID; do
    [ -n "$(printenv "WINDOWS_RUNNER_$v" || true)" ] || missing+=("variable WINDOWS_RUNNER_$v")
  done
  [ "${HAS_APP_KEY:-false}" = true ] || missing+=("secret WINDOWS_RUNNER_APP_PRIVATE_KEY")
  if [ "${#missing[@]}" -gt 0 ]; then
    local list; list=$(IFS=,; echo "${missing[*]}")
    err "the windows-runner environment is not configured (missing:${list//,/, }). Its settings come from the jerboa-infra Windows runner setup; see docs/release-process.md. Set them, then re-run windows-runner-up."
  fi
  echo "windows-runner environment is configured"
}

cmd_launch() {
  : "${ROLE_ARN:?}" "${AWS_REGION:?}" "${LAUNCH_TEMPLATE_ID:?}" "${GH_TOKEN:?}" "${RUNNER_NAME:?}" "${RUNNER_LABEL:?}"
  local out="${GITHUB_OUTPUT:?}" userdata="${RUNNER_TEMP:?}/windows-runner-user-data.txt" jit image instance
  trap 'rm -f "$userdata"' EXIT

  # Single-use registration: the runner takes exactly one job, then deregisters.
  jit=$(gh api -X POST "repos/${repo}/actions/runners/generate-jitconfig" \
    -f name="$RUNNER_NAME" -F runner_group_id=1 \
    -f 'labels[]=self-hosted' -f 'labels[]=windows' -f 'labels[]=x64' -f "labels[]=${RUNNER_LABEL}" \
    -f work_folder=_work --jq .encoded_jit_config)
  if [ -z "$jit" ] || [ "$jit" = null ]; then err "GitHub did not return a runner configuration"; fi
  echo "::add-mask::$jit"
  # Base64 only: it is embedded in a PowerShell single-quoted string.
  [[ "$jit" =~ ^[A-Za-z0-9+/=]+$ ]] || err "unexpected runner configuration format"
  (umask 077; printf "<powershell>& C:\\\\jerboa\\\\start-runner.ps1 -JitConfig '%s'</powershell>\n" "$jit" >"$userdata")
  unset jit

  assume_role
  image=$(aws ec2 describe-images --region "$AWS_REGION" --owners self \
    --filters Name=tag:jerboa:role,Values=ci-windows-runner-image Name=state,Values=available \
    --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)
  case "$image" in
    ami-*) echo "image: $image" ;;
    *) err "no available image tagged jerboa:role=ci-windows-runner-image in $AWS_REGION; build one with the jerboa-infra Windows runner image pipeline" ;;
  esac

  instance=$(aws ec2 run-instances --region "$AWS_REGION" \
    --launch-template "LaunchTemplateId=${LAUNCH_TEMPLATE_ID},Version=\$Default" \
    --image-id "$image" --user-data "file://${userdata}" \
    --tag-specifications "ResourceType=instance,Tags=[{Key=jerboa:role,Value=ci-windows-runner},{Key=jerboa:run,Value=${run_tag}},{Key=Name,Value=${RUNNER_NAME}}]" \
    --query 'Instances[0].InstanceId' --output text)
  case "$instance" in
    i-*) echo "instance_id=$instance" >>"$out"; echo "instance: $instance" ;;
    *) err "run-instances returned no instance" ;;
  esac
  rm -f "$userdata"

  # Wait until the runner registers, so the smoke job is only queued for a
  # machine that exists. The instance stops itself at the latest 90 minutes
  # after boot and windows-runner-down terminates it in any case.
  local status deadline=$((SECONDS + ${RUNNER_BOOT_TIMEOUT:-1200}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    status=$(gh api "repos/${repo}/actions/runners?name=${RUNNER_NAME}" --jq '.runners[0].status // ""')
    if [ "$status" = online ]; then echo "runner $RUNNER_NAME is online"; return 0; fi
    sleep 15
  done
  err "runner $RUNNER_NAME did not come online within $(( ${RUNNER_BOOT_TIMEOUT:-1200} / 60 )) minutes (instance $instance); check its console output in EC2"
}

cmd_terminate() {
  : "${ROLE_ARN:?}" "${AWS_REGION:?}" "${INSTANCE_ID:?}"
  [[ "$INSTANCE_ID" =~ ^i-[0-9a-f]+$ ]] || err "unexpected instance id"
  assume_role
  aws ec2 terminate-instances --region "$AWS_REGION" --instance-ids "$INSTANCE_ID" \
    --query 'TerminatingInstances[0].[InstanceId,PreviousState.Name,CurrentState.Name]' --output text
}

cmd_deregister() {
  : "${GH_TOKEN:?}" "${RUNNER_NAME:?}"
  local id
  # The runner removes itself after its one job; it remains if it never got one.
  for id in $(gh api "repos/${repo}/actions/runners?name=${RUNNER_NAME}" --jq '.runners[].id'); do
    echo "removing runner registration $id ($RUNNER_NAME)"
    gh api -X DELETE "repos/${repo}/actions/runners/${id}" --silent
  done
}

case "${1:-}" in
  check) cmd_check ;;
  launch) cmd_launch ;;
  terminate) cmd_terminate ;;
  deregister) cmd_deregister ;;
  *) echo "usage: $0 check|launch|terminate|deregister" >&2; exit 2 ;;
esac
