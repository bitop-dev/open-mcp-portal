import { Stack } from "@/components/ui/layout/layout";
import { Time } from "@/components/ui/time/time";
import type { Sink } from "./api";

/**
 * The sequence a required full-profile sink is held at, and whether the
 * receiver has been sent that oversized envelope. A delivered envelope is not
 * resent while the hold lasts; a failed delivery is retried with backoff.
 * `held_notice` is absent from replicas that predate it: show the sequence only.
 */
export function HeldOversized({ sink }: { sink: Sink }) {
  if (sink.held_oversized_seq == null) return <>None</>;
  const notice = sink.held_notice;
  return <Stack gap={1}>
    <span>{sink.held_oversized_seq}</span>
    {notice === null && <span>Not yet sent to the receiver.</span>}
    {notice?.delivered_at && <span>Sent to the receiver <Time value={notice.delivered_at} />. Not resent while held.</span>}
    {notice?.failed_at && <span>Sending failed <Time value={notice.failed_at} /> ({notice.failure_class}). Retrying with backoff.</span>}
  </Stack>;
}
