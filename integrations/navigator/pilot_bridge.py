# SPDX-License-Identifier: Apache-2.0
"""Host-owned Navigator Work bridge to the native Go tool provider.

Uses the supported create_persistence_app(work_connector_bridges=...) extension.
Work API authenticates organization/workspace and requires a writer for calls;
the harness retains its existing write approval and durable operation receipts.
This is distinct from the fixed-operation conversation peripheral client.
"""
import json
import os
import re
import uuid
from cyrene_plugin_runtime import DirectPluginClient
from cyrene_tool_provider_contracts import tool_provider_pb2 as wire
from cyrene_tool_provider_contracts import TypedPayload, encode_request, decode_response

TOOLS = frozenset(('pilot_job_status', 'eval_replay_start', 'data_guard_start',
                   'run_doctor_start', 'pilot_guard_apply', 'pilot_doctor_prepare',
                   'pilot_doctor_start', 'pilot_doctor_check'))
READ_ONLY = frozenset(('pilot_job_status', 'pilot_doctor_check'))
IDENTIFIER = re.compile(r'^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$')


class PilotBridge:
    def __init__(self, binding_id, connection_ref, allowed_tools=TOOLS):
        if not IDENTIFIER.fullmatch(binding_id):
            raise ValueError('Invalid Pilot binding ID')
        self.allowed = frozenset(allowed_tools)
        if not self.allowed or not self.allowed <= TOOLS:
            raise ValueError('Unknown Pilot tool in host allowlist')
        self.binding = binding_id
        self.client = DirectPluginClient.for_local_connection_ref(connection_ref)

    def _invoke(self, method, request):
        payload = encode_request(method, request)
        result = self.client.invoke(capability='tool.provider.v1', interface_version='1',
                                    method=method, request=payload, deadline_seconds=180,
                                    request_id='navigator-pilot-' + uuid.uuid4().hex)
        response = decode_response(method, TypedPayload(type_url=result.type_url, value=result.value))
        if response.WhichOneof('result') == 'error':
            raise RuntimeError('Pilot tool rejected the invocation')
        return response

    def list_tools(self):
        response = self._invoke('list_tools', wire.ListToolsRequest(binding_id=self.binding))
        tools = []
        seen = set()
        for tool in response.catalog.tools:
            if tool.binding_id != self.binding or tool.provider_tool_id in seen:
                raise RuntimeError('Pilot returned an invalid catalog binding')
            seen.add(tool.provider_tool_id)
            if tool.provider_tool_id in self.allowed:
                tools.append(dict(id=tool.provider_tool_id, name=tool.display_name,
                                  description=tool.description,
                                  readOnly=tool.provider_tool_id in READ_ONLY,
                                  inputSchema=json.loads(tool.input_schema_json)))
        return {'tools': tools}

    def call_tool(self, tool_id, arguments):
        if tool_id not in self.allowed or not isinstance(arguments, dict):
            raise ValueError('Pilot operation is not authorized by the host')
        raw = json.dumps(arguments, ensure_ascii=False, allow_nan=False)
        if len(raw.encode()) > 1_500_000:
            raise ValueError('Pilot arguments exceed the limit')
        response = self._invoke('call_tool', wire.CallToolRequest(
            binding_id=self.binding, provider_tool_id=tool_id, arguments_json=raw))
        content, structured = [], None
        for part in response.outcome.content:
            kind = part.WhichOneof('content')
            if kind == 'json':
                value = json.loads(part.json.json)
                content.append({'type': 'text', 'text': json.dumps(value, ensure_ascii=False)})
                structured = value
            elif kind == 'text':
                content.append({'type': 'text', 'text': part.text.text})
            else:
                raise RuntimeError('Unsupported Pilot response content')
        return {'content': content, 'structuredContent': structured,
                'isError': response.outcome.is_error}

    def close(self):
        self.client.close()


def attach_pilot(factory, bindings, environment=None):
    """Wrap the public app factory; preserve existing auth, routes and bridges."""
    environment = os.environ if environment is None else environment
    if not isinstance(bindings, list) or not 1 <= len(bindings) <= 128:
        raise ValueError('Pilot bindings must be a bounded non-empty list')
    rows, scopes = [], set()
    for row in bindings:
        if not isinstance(row, dict) or set(row) - {'organization_id', 'workspace_id', 'binding_id', 'connection_ref_env', 'allowed_tools'}:
            raise ValueError('Invalid Pilot binding configuration')
        scope = tuple(row.get(key) for key in ('organization_id', 'workspace_id', 'binding_id'))
        if any(not isinstance(value, str) or not IDENTIFIER.fullmatch(value) for value in scope) or scope in scopes:
            raise ValueError('Duplicate or invalid Pilot scope')
        name = row.get('connection_ref_env')
        if not isinstance(name, str) or not re.fullmatch('[A-Z][A-Z0-9_]*', name) or not environment.get(name):
            raise ValueError('Missing host-owned Pilot connection reference')
        scopes.add(scope)
        rows.append((scope, environment[name], row.get('allowed_tools', TOOLS)))
    # One backend must not be shared by separate tenant scopes: plugin job storage
    # is binding-local, so sharing a connection would expose cross-scope job IDs.
    if len({ref for _, ref, _ in rows}) != len(rows):
        raise ValueError('Each Pilot scope requires a dedicated plugin instance')

    def create(db_path, principals, *args, **kwargs):
        bridges = dict(kwargs.pop('work_connector_bridges', None) or {})
        created = []
        try:
            for scope, ref, tools in rows:
                org, workspace, binding = scope
                if scope in bridges:
                    raise ValueError('Pilot binding collides with an existing bridge')
                if not any(p.organization_id == org and workspace in p.workspace_ids and p.can_takeover
                           for p in principals.values()):
                    raise ValueError('Pilot binding requires a writer in its exact organization/workspace')
                bridge = PilotBridge(binding, ref, tools)
                created.append(bridge)
                bridges[scope] = bridge
            app = factory(db_path, principals, *args, work_connector_bridges=bridges, **kwargs)
            for bridge in created:
                app.router.add_event_handler('shutdown', bridge.close)
            return app
        except BaseException:
            for bridge in created:
                bridge.close()
            raise
    return create
