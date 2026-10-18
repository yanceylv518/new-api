/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { RequestTimelineResult, RequestTimelineWriterStatus } from '../api'

export function writerStatusResult(): RequestTimelineWriterStatus {
  return {
    initialized: true,
    enabled: true,
    accepting: true,
    node_id: 'fixture-node',
    started_at_us: 1_760_509_900_000_000,
    queue_capacity: 512,
    accepted: 10,
    pending: 1,
    processing: 0,
    buffered: 0,
    writing: 1,
    outstanding: 2,
    oldest_outstanding_age_us: 250_000,
    written: 7,
    failed: 1,
    dropped: 3,
    retries: 2,
    dropped_encoding: 1,
    dropped_queue_full: 1,
    dropped_stopped: 1,
    dropped_shutdown: 0,
    last_written_at_us: 1_760_509_910_000_000,
    last_failed_at_us: 1_760_509_908_000_000,
    last_batch_size: 2,
    last_batch_duration_us: 200_123,
    retention_days: 7,
    cleanup_deleted: 3000,
    cleanup_failed: 0,
    cleanup_pending: false,
  }
}

// 一次重试后成功的流式请求：第 1 次复用连接返回 503，第 2 次新建连接成功。
// 偏移单位与接口一致，均为微秒。
export function retriedStreamResult(): RequestTimelineResult {
  return {
    found: true,
    enabled: true,
    retention_days: 7,
    summary: {
      request_id: 'req_retry_1',
      started_at_us: 1_760_509_906_000_000,
      finished_at_us: 1_760_509_913_419_300,
      duration_us: 7_419_300,
      first_payload_write_offset_us: 1_322_700,
      http_status: 200,
      outcome: 'success',
      is_stream: true,
      capture_status: 'complete',
      schema_version: 1,
    },
    detail: {
      attempt_count: 2,
      final_channel_id: 27,
      final_attempt_first_byte_offset_us: 1_305_800,
      node_id: 'node-a',
      status: 'complete',
      timeline: {
        method: 'POST',
        route: '/v1/chat/completions',
        phases: [
          { name: 'auth', start_us: 120, end_us: 980 },
          { name: 'distribute', start_us: 1200, end_us: 7900 },
          { name: 'request_body', start_us: 1350, end_us: 4200 },
          { name: 'precharge', start_us: 12_200, end_us: 16_300 },
          { name: 'channel_select', start_us: 16_350, end_us: 16_900 },
          {
            name: 'channel_handler',
            attempt: 1,
            start_us: 17_000,
            end_us: 316_400,
          },
          { name: 'channel_select', start_us: 316_900, end_us: 320_600 },
          {
            name: 'channel_handler',
            attempt: 2,
            start_us: 320_800,
            end_us: 7_413_000,
          },
          {
            name: 'response_handle',
            attempt: 2,
            start_us: 1_306_000,
            end_us: 7_401_800,
          },
          {
            name: 'settlement',
            attempt: 2,
            start_us: 7_402_000,
            end_us: 7_412_600,
          },
          {
            name: 'consume_log',
            attempt: 2,
            start_us: 7_402_600,
            end_us: 7_412_300,
          },
          {
            name: 'log_insert',
            attempt: 2,
            start_us: 7_403_200,
            end_us: 7_412_000,
          },
        ],
        attempts: [
          {
            index: 1,
            channel_id: 12,
            start_us: 16_950,
            end_us: 316_600,
            outcome: 'failed',
            exchanges: [
              {
                index: 1,
                kind: 'http',
                start_us: 18_700,
                headers_us: 313_400,
                first_byte_us: 313_200,
                end_us: 315_800,
                status: 503,
                protocol: 'HTTP/2.0',
                event_stream: false,
                proxy: false,
                reused: true,
                bytes_read: 412,
                end_reason: 'eof',
                events: [
                  { sequence: 1, name: 'get_conn', offset_us: 18_750 },
                  { sequence: 2, name: 'got_conn', offset_us: 18_900 },
                  { sequence: 3, name: 'wrote_request', offset_us: 24_100 },
                  { sequence: 4, name: 'first_byte', offset_us: 313_200 },
                ],
              },
            ],
          },
          {
            index: 2,
            channel_id: 27,
            start_us: 320_700,
            end_us: 7_413_100,
            outcome: 'success',
            exchanges: [
              {
                index: 1,
                kind: 'http',
                start_us: 322_400,
                headers_us: 1_306_100,
                first_byte_us: 1_305_800,
                end_us: 7_401_600,
                status: 200,
                protocol: 'HTTP/2.0',
                event_stream: true,
                proxy: false,
                reused: false,
                bytes_read: 812_300,
                end_reason: 'eof',
                events: [
                  { sequence: 5, name: 'get_conn', offset_us: 322_450 },
                  { sequence: 6, name: 'dns_start', offset_us: 322_600 },
                  { sequence: 7, name: 'dns_done', offset_us: 324_900 },
                  { sequence: 8, name: 'connect_start', offset_us: 325_000 },
                  { sequence: 9, name: 'connect_done', offset_us: 331_200 },
                  { sequence: 10, name: 'tls_start', offset_us: 331_300 },
                  { sequence: 11, name: 'tls_done', offset_us: 344_600 },
                  { sequence: 12, name: 'got_conn', offset_us: 344_700 },
                  { sequence: 13, name: 'wrote_request', offset_us: 350_000 },
                  { sequence: 14, name: 'first_byte', offset_us: 1_305_800 },
                ],
              },
            ],
            stream: {
              first_us: 1_321_100,
              last_us: 7_401_500,
              count: 241,
              max_gap_us: 163_200,
              end: 'done',
              failed: false,
            },
          },
        ],
        response: {
          bytes_written: 830_514,
          write_failed: false,
          last_write_us: 7_402_100,
          cancel_observed_us: null,
          hijacked: false,
        },
        capture: { truncated: false, missing: [] },
        errors: [],
      },
    },
  }
}
