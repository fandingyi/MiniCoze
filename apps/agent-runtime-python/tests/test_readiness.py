from dataclasses import replace
from fastapi.testclient import TestClient
from runtime.app import create_app
from test_runtime import SETTINGS, TOKEN


def test_readiness_requires_service_identity_and_reports_gateway():
    with TestClient(create_app(SETTINGS)) as client:
        assert client.get('/internal/v1/ready').status_code == 401
        response = client.get('/internal/v1/ready', headers={'Authorization': f'Bearer {TOKEN}'})
        assert response.json() == {'status': 'ready', 'tools_ready': False}
    with TestClient(create_app(replace(SETTINGS, tool_gateway_url='http://gateway.test'))) as client:
        assert client.get('/internal/v1/ready', headers={'Authorization': f'Bearer {TOKEN}'}).json()['tools_ready'] is True


def test_readiness_rejects_closed_client_while_liveness_stays_ok():
    app = create_app(SETTINGS)
    with TestClient(app) as client:
        client.portal.call(app.state.client.aclose)
        assert client.get('/internal/v1/ready', headers={'Authorization': f'Bearer {TOKEN}'}).status_code == 503
        assert client.get('/healthz').status_code == 200
