export type ConfigFieldType = 'string' | 'int' | 'bool' | 'hex';
export type ConfigFieldKind = 'live' | 'static';

export interface ConfigKeyMeta {
  Section: string;
  Kind: ConfigFieldKind;
  Type: ConfigFieldType;
  /**
   * Required keys gate first-launch UX. The server marks LLM_PROVIDER plus
   * the API key for the currently-selected provider as Required; everything
   * else is optional. Omitted (or false) for non-required keys — the Go side
   * uses `json:",omitempty"` so the wire shape can lack the field entirely.
   */
  Required?: boolean;
}

export interface ConfigResponse {
  path: string;
  schema: Record<string, ConfigKeyMeta>;
  values: Record<string, unknown>;
  file_keys: string[];
}

export interface SaveConfigPayload {
  patch: Record<string, unknown>;
}
