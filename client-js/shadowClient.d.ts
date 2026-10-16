// Type declarations for shadowClient.js (Shadow Broker JavaScript client).

export interface ShadowMessage {
  id: number;
  message: string;
  /** Present on pull() results only. */
  created_at?: string;
}

export interface PublishOptions {
  /** 'control' sets X-Broker-Lane (separate rate-limit budget on the broker). */
  lane?: 'control' | 'content';
}

export interface PullOptions {
  /** Return messages with id > since (default 0). */
  since?: number;
  /** Max messages per call (default 100, server max 1000). */
  limit?: number;
}

export interface PullResult {
  messages: ShadowMessage[];
  /** Pass as the next `since`. */
  last_id: number;
}

export interface SubscribeOptions {
  /** Reconnect with backoff when the socket drops (default true). */
  autoReconnect?: boolean;
}

export class ShadowClient {
  /** Reads SHADOW_BROKER_CONFIG (base64 JSON with url and auth_token). */
  constructor();
  publish(queue: string, message: string, options?: PublishOptions): Promise<{ id: number }>;
  pull(queue: string, options?: PullOptions): Promise<PullResult>;
  /**
   * Streams new messages until the process exits. The broker sends no
   * handshake ack and never replays history: pull from your last id after
   * connecting if you need gapless delivery.
   */
  subscribe(queue: string, onMessage: (msg: ShadowMessage) => void, options?: SubscribeOptions): Promise<void>;
  /** Resolves true when the broker answers an application-level ping. */
  ping(): Promise<boolean>;
}
