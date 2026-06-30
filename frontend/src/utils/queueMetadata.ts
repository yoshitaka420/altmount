import type { QueueItem } from "../types/api";

interface QueueMetadata {
	stream_blocklist_blocked?: boolean;
	stream_blocklist_marked_dead?: boolean;
	stream_blocklist_reason?: string;
}

export function getStreamBlocklistQueueNote(item: QueueItem): string | null {
	if (!item.metadata) return null;
	try {
		const metadata = JSON.parse(item.metadata) as QueueMetadata;
		if (metadata.stream_blocklist_blocked) {
			return (
				metadata.stream_blocklist_reason || "This release was blocked by the stream blocklist."
			);
		}
		if (metadata.stream_blocklist_marked_dead) {
			return (
				metadata.stream_blocklist_reason ||
				"This failed Stremio import was added to the stream blocklist."
			);
		}
		return null;
	} catch {
		return null;
	}
}
