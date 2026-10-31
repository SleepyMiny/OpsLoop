"""Heartbeat-only TLS PATCH response custody. No retries or ambient credentials.

The caller supplies one fixed Policy path and a private per-attempt receipt root.
Existing static TLS authentication/redirect restrictions are reused unchanged.
"""
import json,urllib.request,urllib.error
from pathlib import Path
from tools.exhibition_runtime import Transport,canonical,require
from tools.exhibition_lifecycle import write
from tools.exhibition_hashes import parse


def send(settings,path,payload,directory,before_send):
    target='/apis/opsloop.io/v1alpha1/namespaces/'+settings['namespace']+'/recoverypolicies/'+settings['policy']
    require(path==target,'HEARTBEAT_TARGET_CHANGED')
    api=Transport(settings)
    try:
        headers={'Accept':'application/json','Content-Type':'application/json-patch+json'}
        if api.token:headers['Authorization']='Bearer '+api.token
        request=urllib.request.Request(api.origin+path,data=canonical(payload),headers=headers,method='PATCH')
        # The last validity check occurs after TLS/client setup, immediately before I/O.
        before_send()
        write(directory/'dispatch.json',canonical({'state':'DISPATCH_RESERVED','method':'PATCH','path':path}),exclusive=True)
        try:
            with api.opener.open(request,timeout=2) as response:
                code=response.status;raw=response.read(1048577)
        except urllib.error.HTTPError as error:
            with error:code=error.code;raw=error.read(1048577)
        except (OSError,urllib.error.URLError) as error:
            write(directory/'transport-error.json',canonical({'state':'APPLY_UNKNOWN','errorType':type(error).__name__}),exclusive=True)
            return {'disposition':'APPLY_UNKNOWN','httpStatus':None,'status':None,'object':None}
        # Bounded private raw response, without request headers/auth material.
        write(directory/'response.body',raw[:1048576],exclusive=True)
        if len(raw)>1048576:
            return {'disposition':'APPLY_UNKNOWN','httpStatus':code,'status':None,'object':None}
        try:value=parse(raw)
        except Exception:value=None
        if code==200 and isinstance(value,dict) and value.get('kind')=='RecoveryPolicy':
            return {'disposition':'APPLIED_RESPONSE','httpStatus':code,'status':None,'object':value}
        if isinstance(value,dict) and value.get('kind')=='Status' and value.get('status')=='Failure' and value.get('code')==code:
            return {'disposition':'SERVER_REJECTION','httpStatus':code,'status':value,'object':None}
        return {'disposition':'APPLY_UNKNOWN','httpStatus':code,'status':None,'object':None}
    finally:api.close()


def resource_version_conflict(response,policy):
    """Require matching authenticated HTTP + Kubernetes Status + exact target +
    apiserver's optimistic-concurrency reason. 409/text alone never authorizes retry.
    JSON Patch test failures (422), admission errors and lost responses are excluded.
    """
    if not isinstance(response,dict):return False
    status=response.get('status');
    if not isinstance(status,dict):return False
    details=status.get('details')
    if not isinstance(details,dict):return False
    prefix='Operation cannot be fulfilled on recoverypolicies.opsloop.io "'+policy+'": '
    return (response.get('disposition')=='SERVER_REJECTION' and response.get('httpStatus')==409
        and status.get('kind')=='Status' and status.get('status')=='Failure' and status.get('code')==409
        and status.get('reason')=='Conflict' and details.get('group')=='opsloop.io'
        and details.get('kind')=='recoverypolicies' and details.get('name')==policy
        and not details.get('causes')
        and status.get('message')==prefix+'the object has been modified; please apply your changes to the latest version and try again')
