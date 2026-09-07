# Notion n8n Monitor

This repository contains an n8n workflow that monitors a Notion database and calls a local port where OpenCode is running.

## Features

- Monitors Notion database for changes
- Calls local endpoint when changes detected
- Integrates with OpenCode platform

## Plugin Dependencies

1. n8n-nodes-base
2. n8n-nodes-notion
3. n8n-nodes-webhook
4. n8n-nodes-http-request

## Setup Instructions

### Prerequisites

1. Node.js (v14 or higher)
2. Docker (optional, for containerized setup)
3. OpenCode platform running on local port
4. Notion API token
5. Notion database URL

### Installation Steps

#### Option 1: Direct Setup

1. Clone the repository:
```bash
git clone <repository-url>
```

2. Install dependencies:
```bash
npm install
```

3. Set up credentials:
   - Create a Notion API token in your Notion account settings
   - Add the token to the n8n credentials

4. Configure the workflow:
   - Open n8n in your browser
   - Import the flow from `flows/notion-monitor.json`
   - Update the Notion database URL and local endpoint URL in the webhook node

5. Start n8n:
```bash
n8n
```

6. Monitor the workflow:
   - The workflow will automatically check for changes in your Notion database
   - When changes are detected, it will make a request to your OpenCode local endpoint

#### Option 2: Docker Setup (Recommended)

1. Build the Docker image:
```bash
docker build -t notion-n8n-monitor .
```

2. Run the container with necessary environment variables:
```bash
docker run -d \
  --name notion-n8n-monitor \
  -p 5678:5678 \
  -v $(pwd)/flows:/app/flows \
  -v $(pwd)/credentials:/app/credentials \
  -e NOTION_API_TOKEN="your-notion-token-here" \
  -e OPENCODE_BASE_URL="http://host.docker.internal:3000" \
  notion-n8n-monitor
```

### Environment Configuration

Create a `.env` file by copying the example:
```bash
cp .env.example .env
```

Then update the values in `.env` with your actual configuration.

### How It Works

1. Notion node polls the specified database at regular intervals
2. When changes are detected, it triggers the workflow
3. HTTP Request node sends data to your OpenCode local port
4. OpenCode receives and processes the data

## Repository Structure

```
├── flows/
│   └── notion-monitor.json
├── nodes/
├── credentials/
├── package.json
├── Dockerfile
└── README.md
```

## Contributing

Contributions are welcome! Please submit a pull request with any improvements or bug fixes.

## License

This project is licensed under the MIT License.