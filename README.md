# AFM-based RCA Agent (POC)

Replaces the Python RCA agent with an [AFM](https://wso2.github.io/agent-flavored-markdown/) definition + a thin Go sidecar for async execution and report storage.

## Setup

### 1. Install OpenChoreo with RCA enabled

Follow the [OpenChoreo install guide](https://github.com/openchoreo/openchoreo) with RCA enabled (`rca.enabled=true` in the observability plane values).

### 2. Get a bearer token

```bash
TOKEN=$(curl -sk -X POST "http://thunder.openchoreo.localhost:8080/oauth2/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=openchoreo-rca-agent" \
  -d "client_secret=openchoreo-rca-agent-secret" \
  | jq -r '.access_token')
```

### 3. Deploy the AFM agent

```bash
helm install afm-rca ./helm -n openchoreo-observability-plane \
  --set-file agentDefinition=agents/rca-agent.afm.md \
  --set env.MODEL_NAME="gpt-5" \
  --set env.MODEL_PROVIDER="openai" \
  --set env.LLM_API_KEY="sk-..." \
  --set env.MCP_AUTH_TOKEN="$TOKEN" \
  --set env.OBSERVER_MCP_URL="http://observer:8080/mcp" \
  --set env.OPENCHOREO_MCP_URL="http://openchoreo-api.openchoreo-control-plane.svc.cluster.local:8080/mcp"
```

### 4. Point observer to AFM agent

```bash
kubectl patch configmap observer-config -n openchoreo-observability-plane \
  --type merge -p '{"data":{"RCA_SERVICE_URL":"http://afm-rca-afm-agent:8090"}}'

kubectl rollout restart deployment observer -n openchoreo-observability-plane
```

### 5. Point UI reports route to AFM agent

```bash
kubectl patch httproute rca-agent -n openchoreo-observability-plane --type json -p '[
  {"op":"replace","path":"/spec/rules/1/backendRefs/0/name","value":"afm-rca-afm-agent"},
  {"op":"replace","path":"/spec/rules/1/backendRefs/0/port","value":8090}
]'
```

### 6. Trigger an alert

Follow the instructions in the [url-shortener sample](https://github.com/openchoreo/openchoreo/tree/main/samples/from-image/url-shortener) to deploy it and trigger an alert.

## Refreshing the token

> **Note:** The token expires in ~1 hour. When it expires, MCP calls will start failing. Re-run the steps below to get a new token.

```bash
TOKEN=$(curl -sk -X POST "http://thunder.openchoreo.localhost:8080/oauth2/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=client_credentials" \
  -d "client_id=openchoreo-rca-agent" \
  -d "client_secret=openchoreo-rca-agent-secret" \
  | jq -r '.access_token')

helm upgrade afm-rca ./helm -n openchoreo-observability-plane \
  --reuse-values \
  --set-file agentDefinition=agents/rca-agent.afm.md \
  --set env.MCP_AUTH_TOKEN="$TOKEN"
```
