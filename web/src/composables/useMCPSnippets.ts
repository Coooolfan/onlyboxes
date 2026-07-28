import { computed, type ComputedRef, type Ref } from 'vue'

export type MCPSnippetKey = 'claude-code' | 'http-header' | 'mcp-json'

export interface MCPSnippet {
  key: MCPSnippetKey
  label: string
  kind: 'Command' | 'Parameter'
  value: string
}

const fallbackConsoleOrigin = 'http://127.0.0.1:8089'

function resolveMCPURL(): string {
  const origin =
    typeof window !== 'undefined' && window.location?.origin
      ? window.location.origin
      : fallbackConsoleOrigin
  return new URL('/mcp', origin).toString()
}

/**
 * Ready-to-paste MCP client configuration snippets for a freshly created
 * trusted token.
 */
export function useMCPSnippets(
  token: Ref<string> | ComputedRef<string>,
): ComputedRef<MCPSnippet[]> {
  return computed(() => {
    const tokenValue = token.value.trim()
    if (!tokenValue) {
      return []
    }

    const mcpURL = resolveMCPURL()
    const authorizationHeader = `Authorization: Bearer ${tokenValue}`

    return [
      {
        key: 'claude-code',
        label: 'claude code',
        kind: 'Command',
        value: `claude mcp add --transport http onlyboxes "${mcpURL}" --header "${authorizationHeader}"`,
      },
      {
        key: 'http-header',
        label: 'http header',
        kind: 'Parameter',
        value: authorizationHeader,
      },
      {
        key: 'mcp-json',
        label: 'mcp json',
        kind: 'Parameter',
        value: JSON.stringify(
          {
            mcpServers: {
              onlyboxes: {
                url: mcpURL,
                headers: { Authorization: `Bearer ${tokenValue}` },
              },
            },
          },
          null,
          2,
        ),
      },
    ]
  })
}
