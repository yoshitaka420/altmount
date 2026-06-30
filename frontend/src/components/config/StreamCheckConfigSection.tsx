import {
	Activity,
	Database,
	Download,
	FileUp,
	RefreshCw,
	Save,
	ShieldCheck,
	Trash2,
	Upload,
} from "lucide-react";
import { forwardRef, useCallback, useEffect, useImperativeHandle, useState } from "react";
import { APIError, apiClient } from "../../api/client";
import { useToast } from "../../contexts/ToastContext";
import type { StreamBlocklistSourceInfo, StreamBlocklistSourcesResponse, StreamBlocklistTrust } from "../../types/api";
import type {
	ConfigResponse,
	StreamCheckConfig,
	StreamCheckStreamBlocklistConfig,
} from "../../types/config";
import { LoadingSpinner } from "../ui/LoadingSpinner";

interface StreamCheckConfigSectionProps {
	config: ConfigResponse;
	onUpdate?: (section: string, data: StreamCheckConfig) => Promise<void>;
	isReadOnly?: boolean;
	isUpdating?: boolean;
	hideSaveButton?: boolean;
	onDirtyChange?: (hasChanges: boolean) => void;
}

export interface StreamCheckConfigSectionHandle {
	hasChanges: boolean;
	save: () => Promise<void>;
}

const DEFAULT_STREAM_BLOCKLIST: StreamCheckStreamBlocklistConfig = {
	enabled: true,
	quorum: 2,
	max_source_entries: 2000000,
	backbone_scope: true,
	mark_dead: true,
};

const DEFAULTS: StreamCheckConfig = {
	enabled: false,
	segment_sample_percentage: 5,
	max_connections: 10,
	timeout_seconds: 15,
	acceptable_missing_percentage: 0,
	cache_ttl_minutes: 30,
	max_batch: 50,
	stream_blocklist: DEFAULT_STREAM_BLOCKLIST,
};

const DEFAULT_SOURCE_TRUST: StreamBlocklistTrust = "full";

function resolveStreamBlocklistConfig(c: StreamCheckStreamBlocklistConfig | undefined): StreamCheckStreamBlocklistConfig {
	return {
		enabled: c?.enabled ?? DEFAULT_STREAM_BLOCKLIST.enabled,
		db_path: c?.db_path ?? DEFAULT_STREAM_BLOCKLIST.db_path,
		quorum: c?.quorum && c.quorum > 0 ? c.quorum : DEFAULT_STREAM_BLOCKLIST.quorum,
		max_source_entries:
			c?.max_source_entries && c.max_source_entries > 0
				? c.max_source_entries
				: DEFAULT_STREAM_BLOCKLIST.max_source_entries,
		backbone_scope: c?.backbone_scope ?? DEFAULT_STREAM_BLOCKLIST.backbone_scope,
		mark_dead: c?.mark_dead ?? DEFAULT_STREAM_BLOCKLIST.mark_dead,
	};
}

function resolveConfig(c: StreamCheckConfig | undefined): StreamCheckConfig {
	return {
		enabled: c?.enabled ?? DEFAULTS.enabled,
		segment_sample_percentage: c?.segment_sample_percentage ?? DEFAULTS.segment_sample_percentage,
		max_connections: c?.max_connections ?? DEFAULTS.max_connections,
		timeout_seconds: c?.timeout_seconds ?? DEFAULTS.timeout_seconds,
		acceptable_missing_percentage:
			c?.acceptable_missing_percentage ?? DEFAULTS.acceptable_missing_percentage,
		cache_ttl_minutes: c?.cache_ttl_minutes ?? DEFAULTS.cache_ttl_minutes,
		max_batch: c?.max_batch ?? DEFAULTS.max_batch,
		stream_blocklist: resolveStreamBlocklistConfig(c?.stream_blocklist),
	};
}

function clampNum(raw: string, lo: number, hi: number): number {
	const n = Number(raw);
	if (!Number.isFinite(n)) return lo;
	return Math.min(hi, Math.max(lo, n));
}

function formatCount(value: number | undefined): string {
	return new Intl.NumberFormat().format(value ?? 0);
}

function formatTime(value: number): string {
	if (!value) return "Never";
	return new Intl.DateTimeFormat(undefined, {
		dateStyle: "medium",
		timeStyle: "short",
	}).format(new Date(value * 1000));
}

function apiErrorMessage(error: unknown): string {
	if (error instanceof APIError) {
		return error.details || error.message;
	}
	return error instanceof Error ? error.message : "Request failed";
}

function downloadBlob(blob: Blob, filename: string) {
	const url = window.URL.createObjectURL(blob);
	const link = document.createElement("a");
	link.href = url;
	link.download = filename;
	document.body.appendChild(link);
	link.click();
	document.body.removeChild(link);
	window.URL.revokeObjectURL(url);
}

export const StreamCheckConfigSection = forwardRef<
	StreamCheckConfigSectionHandle,
	StreamCheckConfigSectionProps
>(function StreamCheckConfigSection(
	{
		config,
		onUpdate,
		isReadOnly = false,
		isUpdating = false,
		hideSaveButton = false,
		onDirtyChange,
	}: StreamCheckConfigSectionProps,
	ref,
) {
	const { showToast } = useToast();
	const [formData, setFormData] = useState<StreamCheckConfig>(resolveConfig(config.stream_check));
	const [hasChanges, setHasChanges] = useState(false);
	const [streamBlocklistSources, setStreamBlocklistSources] = useState<StreamBlocklistSourcesResponse | null>(null);
	const [streamBlocklistLoading, setStreamBlocklistLoading] = useState(false);
	const [streamBlocklistAction, setStreamBlocklistAction] = useState<string | null>(null);
	const [sourceRefreshHours, setSourceRefreshHours] = useState(24);
	const [sourceImportText, setSourceImportText] = useState("");
	const [sourceImportFile, setSourceImportFile] = useState<File | null>(null);

	const loadStreamBlocklistSources = useCallback(async () => {
		setStreamBlocklistLoading(true);
		try {
			setStreamBlocklistSources(await apiClient.getStreamBlocklistSources());
		} catch (error) {
			showToast({
				type: "error",
				title: "Stream Blocklist Sources Failed",
				message: apiErrorMessage(error),
			});
		} finally {
			setStreamBlocklistLoading(false);
		}
	}, [showToast]);

	useEffect(() => {
		setFormData(resolveConfig(config.stream_check));
		setHasChanges(false);
	}, [config.stream_check]);

	useEffect(() => {
		onDirtyChange?.(hasChanges);
	}, [hasChanges, onDirtyChange]);

	useEffect(() => {
		void loadStreamBlocklistSources();
	}, [loadStreamBlocklistSources]);

	const update = (patch: Partial<StreamCheckConfig>) => {
		const updated = { ...formData, ...patch };
		setFormData(updated);
		setHasChanges(JSON.stringify(updated) !== JSON.stringify(resolveConfig(config.stream_check)));
	};

	const runStreamBlocklistAction = async (
		key: string,
		title: string,
		action: () => Promise<string | undefined>,
		refresh = true,
	) => {
		setStreamBlocklistAction(key);
		try {
			const message = await action();
			if (refresh) {
				await loadStreamBlocklistSources();
			}
			showToast({
				type: "success",
				title,
				message,
			});
		} catch (error) {
			showToast({
				type: "error",
				title: "Stream Blocklist Action Failed",
				message: apiErrorMessage(error),
			});
		} finally {
			setStreamBlocklistAction(null);
		}
	};

	const handleSave = useCallback(async () => {
		if (onUpdate && hasChanges) {
			await onUpdate("stream_check", formData);
			setHasChanges(false);
		}
	}, [formData, hasChanges, onUpdate]);

	useImperativeHandle(
		ref,
		() => ({
			hasChanges,
			save: handleSave,
		}),
		[hasChanges, handleSave],
	);

	const handleClearLocal = async () => {
		if (!window.confirm("Clear entries from My list?")) return;
		await runStreamBlocklistAction("clear-local", "My List Cleared", async () => {
			const result = await apiClient.clearStreamBlocklistLocal();
			return `${formatCount(result.cleared)} entries removed.`;
		});
	};

	const handleExportStreamBlocklistList = async (scope: "local" | "merged") => {
		await runStreamBlocklistAction(
			`export-${scope}`,
			"Stream Blocklist Export Ready",
			async () => {
				const blob = await apiClient.exportStreamBlocklistList(scope);
				downloadBlob(
					blob,
					scope === "merged" ? "stream-blocklist-merged.ndjson.gz" : "stream-blocklist.ndjson.gz",
				);
				return "Download started.";
			},
			false,
		);
	};

	const handleImportSources = async () => {
		if (!sourceImportText.trim() && !sourceImportFile) {
			showToast({
				type: "warning",
				title: "Add Source Entries",
				message: "Paste source URLs, paste a bundle, or choose a bundle file.",
			});
			return;
		}
		await runStreamBlocklistAction("import-sources", "Stream Blocklist Sources Imported", async () => {
			const result = await apiClient.importStreamBlocklistSources({
				text: sourceImportText.trim() || undefined,
				file: sourceImportFile || undefined,
				trust: DEFAULT_SOURCE_TRUST,
				refreshHours: sourceRefreshHours,
			});
			setSourceImportText("");
			setSourceImportFile(null);
			return `${formatCount(result.added)} added, ${formatCount(result.skipped)} skipped, ${formatCount(result.invalid)} invalid.`;
		});
	};

	const handleExportSources = async () => {
		await runStreamBlocklistAction(
			"export-sources",
			"Stream Blocklist Sources Export Ready",
			async () => {
				const blob = await apiClient.exportStreamBlocklistSources();
				downloadBlob(blob, "stream-blocklist-sources.json");
				return "Download started.";
			},
			false,
		);
	};

	const handleUpdateSource = async (
		source: StreamBlocklistSourceInfo,
		patch: { enabled?: boolean; refreshHours?: number },
	) => {
		await runStreamBlocklistAction(`source-${source.id}`, "Stream Blocklist Source Updated", async () => {
			await apiClient.updateStreamBlocklistSource({ id: source.id, ...patch });
			return undefined;
		});
	};

	const handleRefreshSource = async (source: StreamBlocklistSourceInfo) => {
		await runStreamBlocklistAction(`refresh-${source.id}`, "Stream Blocklist Source Refreshed", async () => {
			const result = await apiClient.refreshStreamBlocklistSource(source.id);
			return result.message || "Refresh completed.";
		});
	};

	const handleRemoveSource = async (source: StreamBlocklistSourceInfo) => {
		if (!window.confirm(`Remove ${source.name}?`)) return;
		await runStreamBlocklistAction(`remove-${source.id}`, "Stream Blocklist Source Removed", async () => {
			const result = await apiClient.removeStreamBlocklistSource(source.id);
			return `${formatCount(result.removed)} source removed.`;
		});
	};

	const sourceRows = streamBlocklistSources?.sources ?? [];
	const busy = streamBlocklistAction !== null;

	return (
		<div className="min-w-0 space-y-10">
			<div className="min-w-0 space-y-8">
				<div className="min-w-0 space-y-6 overflow-hidden rounded-2xl border-2 border-base-300/80 bg-base-200/60 p-6">
					<div className="flex items-center gap-2">
						<Activity className="h-4 w-4 text-base-content/60" />
						<h4 className="font-bold text-base-content/40 text-xs uppercase tracking-widest">
							Endpoint
						</h4>
						<div className="h-px flex-1 bg-base-300/50" />
					</div>

					<div className="flex items-center justify-between gap-4">
						<div className="min-w-0 flex-1">
							<h5 className="break-words font-bold text-sm">Enable Stream Check</h5>
							<p className="mt-2 block max-w-3xl break-words text-[11px] text-base-content/50 leading-relaxed">
								Activates the POST /api/nzb/check endpoint. Clients send an NZB URL and receive an
								availability verdict before playback.
							</p>
						</div>
						<input
							type="checkbox"
							className="toggle toggle-primary mt-1 shrink-0"
							checked={formData.enabled}
							disabled={isReadOnly}
							onChange={(e) => update({ enabled: e.target.checked })}
						/>
					</div>
				</div>

				{formData.enabled && (
					<div className="fade-in slide-in-from-top-2 min-w-0 animate-in space-y-6 overflow-hidden rounded-2xl border-2 border-base-300/80 bg-base-200/60 p-6">
						<div className="flex items-center gap-2">
							<Activity className="h-4 w-4 text-base-content/60" />
							<h4 className="font-bold text-base-content/40 text-xs uppercase tracking-widest">
								Verification
							</h4>
							<div className="h-px flex-1 bg-base-300/50" />
						</div>

						<div className="grid min-w-0 grid-cols-1 gap-6 sm:grid-cols-2">
							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">Segment Sample %</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={1}
									max={100}
									value={formData.segment_sample_percentage}
									disabled={isReadOnly}
									onChange={(e) =>
										update({ segment_sample_percentage: clampNum(e.target.value, 1, 100) })
									}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									Percentage of release segments to STAT-sample. Lower values are faster and
									cheaper.
								</p>
							</fieldset>

							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">Max Connections</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={1}
									max={100}
									value={formData.max_connections}
									disabled={isReadOnly}
									onChange={(e) => update({ max_connections: clampNum(e.target.value, 1, 100) })}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									Concurrent NNTP STAT requests per check.
								</p>
							</fieldset>

							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">STAT Timeout (seconds)</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={1}
									max={600}
									value={formData.timeout_seconds}
									disabled={isReadOnly}
									onChange={(e) => update({ timeout_seconds: clampNum(e.target.value, 1, 600) })}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									Per-segment STAT timeout before a segment is treated as missing.
								</p>
							</fieldset>

							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">Acceptable Missing %</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={0}
									max={100}
									step={0.5}
									value={formData.acceptable_missing_percentage}
									disabled={isReadOnly}
									onChange={(e) =>
										update({ acceptable_missing_percentage: clampNum(e.target.value, 0, 100) })
									}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									Share of sampled segments allowed missing before a release is reported dead rather
									than degraded.
								</p>
							</fieldset>

							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">Cache TTL (minutes)</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={0}
									value={formData.cache_ttl_minutes}
									disabled={isReadOnly}
									onChange={(e) =>
										update({ cache_ttl_minutes: clampNum(e.target.value, 0, 100000) })
									}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									How long a verdict is cached to avoid re-checking the same release. Use 0 to
									disable caching.
								</p>
							</fieldset>

							<fieldset className="fieldset min-w-0">
								<legend className="fieldset-legend">Max Batch Size</legend>
								<input
									type="number"
									className="input w-full min-w-0 max-w-full"
									min={1}
									max={1000}
									value={formData.max_batch}
									disabled={isReadOnly}
									onChange={(e) => update({ max_batch: clampNum(e.target.value, 1, 1000) })}
								/>
								<p className="mt-2 block max-w-prose break-words text-base-content/50 text-xs leading-relaxed">
									Maximum number of NZBs verified per request.
								</p>
							</fieldset>
						</div>
					</div>
				)}

				<div className="fade-in slide-in-from-top-2 min-w-0 animate-in space-y-8 overflow-hidden rounded-2xl border-2 border-base-300/80 bg-base-200/60 p-6">
					<div className="flex flex-wrap items-center gap-3">
						<div className="flex min-w-0 flex-1 items-center gap-2">
							<ShieldCheck className="h-4 w-4 text-base-content/60" />
							<h4 className="font-bold text-base-content/40 text-xs uppercase tracking-widest">
								Stream Blocklist
							</h4>
							<div className="h-px flex-1 bg-base-300/50" />
						</div>
						<button
							type="button"
							className="btn btn-ghost btn-sm"
							onClick={loadStreamBlocklistSources}
							disabled={streamBlocklistLoading || busy}
						>
							{streamBlocklistLoading ? <LoadingSpinner size="sm" /> : <RefreshCw className="h-4 w-4" />}
							Refresh
						</button>
					</div>

					<div className="grid min-w-0 grid-cols-1 gap-6 sm:grid-cols-3">
						<div className="min-w-0">
							<div className="text-base-content/50 text-xs uppercase tracking-widest">Local</div>
							<div className="mt-1 truncate font-bold text-2xl">
								{formatCount(streamBlocklistSources?.localCount)}
							</div>
						</div>
						<div className="min-w-0">
							<div className="text-base-content/50 text-xs uppercase tracking-widest">
								Effective
							</div>
							<div className="mt-1 truncate font-bold text-2xl">
								{formatCount(streamBlocklistSources?.effectiveCount)}
							</div>
						</div>
						<div className="min-w-0">
							<div className="text-base-content/50 text-xs uppercase tracking-widest">Rows</div>
							<div className="mt-1 truncate font-bold text-2xl">
								{formatCount(streamBlocklistSources?.totalRows)}
							</div>
						</div>
					</div>

					<div className="grid min-w-0 grid-cols-1 gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(16rem,1fr)]">
						<div className="min-w-0 space-y-5">
							<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
								<FileUp className="h-4 w-4 text-base-content/60" />
								<h5 className="font-bold text-sm">Import Source Bundle</h5>
							</div>

							<div className="grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2">
								<fieldset className="fieldset min-w-0 sm:col-span-2">
									<legend className="fieldset-legend">Source Entries</legend>
									<textarea
										className="textarea min-h-32 w-full min-w-0 max-w-full"
										value={sourceImportText}
										placeholder="Paste stream blocklist source URLs, one per line, or a source bundle JSON."
										disabled={isReadOnly || busy}
										onChange={(e) => setSourceImportText(e.target.value)}
									/>
								</fieldset>

								<fieldset className="fieldset min-w-0 sm:col-span-2">
									<legend className="fieldset-legend">Bundle File</legend>
									<input
										type="file"
										className="file-input w-full min-w-0 max-w-full"
										accept=".json,.txt,application/json,text/plain"
										disabled={isReadOnly || busy}
										onChange={(e) => setSourceImportFile(e.target.files?.[0] ?? null)}
									/>
								</fieldset>

								<fieldset className="fieldset min-w-0">
									<legend className="fieldset-legend">Refresh Hours</legend>
									<input
										type="number"
										className="input w-full min-w-0 max-w-full"
										min={1}
										max={8760}
										value={sourceRefreshHours}
										disabled={isReadOnly || busy}
										onChange={(e) => setSourceRefreshHours(clampNum(e.target.value, 1, 8760))}
									/>
								</fieldset>
							</div>

							<div className="flex flex-wrap gap-2">
								<button
									type="button"
									className="btn btn-primary btn-sm"
									onClick={handleImportSources}
									disabled={isReadOnly || busy || (!sourceImportText.trim() && !sourceImportFile)}
								>
									{streamBlocklistAction === "import-sources" ? (
										<LoadingSpinner size="sm" />
									) : (
										<Upload className="h-4 w-4" />
									)}
									Import Sources
								</button>
								<button
									type="button"
									className="btn btn-ghost btn-sm"
									onClick={handleExportSources}
									disabled={busy}
								>
									<Download className="h-4 w-4" />
									Export Sources
								</button>
							</div>
						</div>

						<div className="min-w-0 space-y-5">
							<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
								<Download className="h-4 w-4 text-base-content/60" />
								<h5 className="font-bold text-sm">List Actions</h5>
							</div>

							<div className="flex flex-col gap-3">
								<button
									type="button"
									className="btn btn-outline min-h-12 w-full justify-start gap-2 border-primary/40 text-primary hover:border-primary hover:bg-primary/10"
									onClick={() => handleExportStreamBlocklistList("merged")}
									disabled={busy}
								>
									<Download className="h-5 w-5" />
									Export Merged
								</button>
								<button
									type="button"
									className="btn btn-outline min-h-12 w-full justify-start gap-2 border-primary/40 text-primary hover:border-primary hover:bg-primary/10"
									onClick={() => handleExportStreamBlocklistList("local")}
									disabled={busy}
								>
									<Download className="h-5 w-5" />
									Export My List
								</button>
								<button
									type="button"
									className="btn btn-outline min-h-12 w-full justify-start gap-2 border-error/50 text-error hover:border-error hover:bg-error/10"
									onClick={handleClearLocal}
									disabled={isReadOnly || busy}
								>
									<Trash2 className="h-5 w-5" />
									Clear My List
								</button>
							</div>
						</div>
					</div>

					<div className="min-w-0 space-y-3">
						<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
							<Database className="h-4 w-4 text-base-content/60" />
							<h5 className="font-bold text-sm">Sources</h5>
						</div>

						<div className="overflow-x-auto rounded-xl border border-base-300/70">
							<table className="table table-sm">
								<thead>
									<tr>
										<th>Name</th>
										<th>Kind</th>
										<th>Rows</th>
										<th>Last Checked</th>
										<th>Status</th>
										<th>Enabled</th>
										<th>Refresh</th>
										<th />
									</tr>
								</thead>
								<tbody>
									{sourceRows.length === 0 && (
										<tr>
											<td colSpan={8} className="py-8 text-center text-base-content/50">
												{streamBlocklistLoading ? "Loading sources..." : "No stream blocklist sources yet."}
											</td>
										</tr>
									)}
									{sourceRows.map((source) => {
										const sourceBusy =
											streamBlocklistAction === `source-${source.id}` ||
											streamBlocklistAction === `refresh-${source.id}` ||
											streamBlocklistAction === `remove-${source.id}`;
										const isLocal = source.kind === "local";
										const isRemote = source.kind === "remote" && Boolean(source.url);

										return (
											<tr key={source.id}>
												<td className="max-w-64">
													<div className="truncate font-semibold">{source.name}</div>
													{source.url && (
														<div className="truncate text-base-content/50 text-xs">
															{source.url}
														</div>
													)}
												</td>
												<td className="capitalize">{source.kind}</td>
												<td>{formatCount(source.count)}</td>
												<td>{formatTime(source.lastChecked)}</td>
												<td className="max-w-48">
													<span className="block truncate text-base-content/70 text-xs">
														{source.status || "-"}
													</span>
												</td>
												<td>
													<input
														type="checkbox"
														className="toggle toggle-sm toggle-primary"
														checked={source.enabled}
														disabled={isReadOnly || busy || isLocal}
														onChange={(e) =>
															handleUpdateSource(source, { enabled: e.target.checked })
														}
													/>
												</td>
												<td>
													<input
														type="number"
														className="input input-sm w-24"
														min={1}
														max={8760}
														defaultValue={source.refreshHours}
														disabled={isReadOnly || busy || isLocal}
														onBlur={(e) => {
															const refreshHours = clampNum(e.target.value, 1, 8760);
															if (refreshHours !== source.refreshHours) {
																void handleUpdateSource(source, { refreshHours });
															}
														}}
													/>
												</td>
												<td>
													<div className="flex justify-end gap-1">
														<button
															type="button"
															className="btn btn-ghost btn-xs"
															onClick={() => handleRefreshSource(source)}
															disabled={isReadOnly || busy || !isRemote}
															title="Refresh source"
														>
															{streamBlocklistAction === `refresh-${source.id}` ? (
																<LoadingSpinner size="sm" />
															) : (
																<RefreshCw className="h-3.5 w-3.5" />
															)}
														</button>
														<button
															type="button"
															className="btn btn-ghost btn-xs text-error"
															onClick={() => handleRemoveSource(source)}
															disabled={isReadOnly || busy || isLocal || sourceBusy}
															title="Remove source"
														>
															<Trash2 className="h-3.5 w-3.5" />
														</button>
													</div>
												</td>
											</tr>
										);
									})}
								</tbody>
							</table>
						</div>
					</div>
				</div>
			</div>

			{!isReadOnly && !hideSaveButton && (
				<div className="flex justify-end border-base-200 border-t pt-4">
					<button
						type="button"
						className={`btn btn-primary px-10 shadow-lg shadow-primary/20 ${!hasChanges && "btn-ghost border-base-300"}`}
						onClick={handleSave}
						disabled={!hasChanges || isUpdating}
					>
						{isUpdating ? <LoadingSpinner size="sm" /> : <Save className="h-4 w-4" />}
						{isUpdating ? "Saving..." : "Save Changes"}
					</button>
				</div>
			)}
		</div>
	);
});
