import {
	Activity,
	Database,
	Download,
	FileUp,
	Link as LinkIcon,
	Plus,
	RefreshCw,
	Save,
	ShieldCheck,
	Trash2,
	Upload,
} from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { APIError, apiClient } from "../../api/client";
import { useToast } from "../../contexts/ToastContext";
import type { WardenSourceInfo, WardenSourcesResponse } from "../../types/api";
import type {
	ConfigResponse,
	StreamCheckConfig,
	StreamCheckWardenConfig,
} from "../../types/config";
import { LoadingSpinner } from "../ui/LoadingSpinner";

interface StreamCheckConfigSectionProps {
	config: ConfigResponse;
	onUpdate?: (section: string, data: StreamCheckConfig) => Promise<void>;
	isReadOnly?: boolean;
	isUpdating?: boolean;
}

const DEFAULT_WARDEN: StreamCheckWardenConfig = {
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
	warden: DEFAULT_WARDEN,
};

function resolveWardenConfig(c: StreamCheckWardenConfig | undefined): StreamCheckWardenConfig {
	return {
		enabled: c?.enabled ?? DEFAULT_WARDEN.enabled,
		db_path: c?.db_path ?? DEFAULT_WARDEN.db_path,
		quorum: c?.quorum ?? DEFAULT_WARDEN.quorum,
		max_source_entries: c?.max_source_entries ?? DEFAULT_WARDEN.max_source_entries,
		backbone_scope: c?.backbone_scope ?? DEFAULT_WARDEN.backbone_scope,
		mark_dead: c?.mark_dead ?? DEFAULT_WARDEN.mark_dead,
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
		warden: resolveWardenConfig(c?.warden),
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

export function StreamCheckConfigSection({
	config,
	onUpdate,
	isReadOnly = false,
	isUpdating = false,
}: StreamCheckConfigSectionProps) {
	const { showToast } = useToast();
	const [formData, setFormData] = useState<StreamCheckConfig>(resolveConfig(config.stream_check));
	const [hasChanges, setHasChanges] = useState(false);
	const [wardenSources, setWardenSources] = useState<WardenSourcesResponse | null>(null);
	const [wardenLoading, setWardenLoading] = useState(false);
	const [wardenAction, setWardenAction] = useState<string | null>(null);
	const [wardenListFile, setWardenListFile] = useState<File | null>(null);
	const [wardenListName, setWardenListName] = useState("");
	const [wardenListTarget, setWardenListTarget] = useState<"local" | "separate">("separate");
	const [sourceURL, setSourceURL] = useState("");
	const [sourceName, setSourceName] = useState("");
	const [sourceRefreshHours, setSourceRefreshHours] = useState(24);
	const [sourceImportText, setSourceImportText] = useState("");
	const [sourceImportFile, setSourceImportFile] = useState<File | null>(null);

	const loadWardenSources = useCallback(async () => {
		setWardenLoading(true);
		try {
			setWardenSources(await apiClient.getWardenSources());
		} catch (error) {
			showToast({
				type: "error",
				title: "Stream Blocklist Sources Failed",
				message: apiErrorMessage(error),
			});
		} finally {
			setWardenLoading(false);
		}
	}, [showToast]);

	useEffect(() => {
		setFormData(resolveConfig(config.stream_check));
		setHasChanges(false);
	}, [config.stream_check]);

	useEffect(() => {
		if (formData.enabled) {
			void loadWardenSources();
		}
	}, [formData.enabled, loadWardenSources]);

	const update = (patch: Partial<StreamCheckConfig>) => {
		const updated = { ...formData, ...patch };
		setFormData(updated);
		setHasChanges(JSON.stringify(updated) !== JSON.stringify(resolveConfig(config.stream_check)));
	};

	const updateWarden = (patch: Partial<StreamCheckWardenConfig>) => {
		update({ warden: { ...formData.warden, ...patch } });
	};

	const runWardenAction = async (
		key: string,
		title: string,
		action: () => Promise<string | undefined>,
		refresh = true,
	) => {
		setWardenAction(key);
		try {
			const message = await action();
			if (refresh) {
				await loadWardenSources();
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
			setWardenAction(null);
		}
	};

	const handleSave = async () => {
		if (onUpdate && hasChanges) {
			await onUpdate("stream_check", formData);
			setHasChanges(false);
		}
	};

	const handleImportWardenList = async () => {
		if (!wardenListFile) {
			showToast({
				type: "warning",
				title: "Choose a Stream Blocklist",
				message: "Select an ndjson or gzip list file first.",
			});
			return;
		}
		await runWardenAction("import-list", "Stream Blocklist Imported", async () => {
			const result = await apiClient.importWardenList({
				file: wardenListFile,
				target: wardenListTarget,
				name: wardenListName.trim() || undefined,
				trust: "full",
			});
			setWardenListFile(null);
			setWardenListName("");
			return `${formatCount(result.added)} entries imported.`;
		});
	};

	const handleClearLocal = async () => {
		if (!window.confirm("Clear entries from My list?")) return;
		await runWardenAction("clear-local", "My List Cleared", async () => {
			const result = await apiClient.clearWardenLocal();
			return `${formatCount(result.cleared)} entries removed.`;
		});
	};

	const handleExportWardenList = async (scope: "local" | "merged") => {
		await runWardenAction(
			`export-${scope}`,
			"Stream Blocklist Export Ready",
			async () => {
				const blob = await apiClient.exportWardenList(scope);
				downloadBlob(
					blob,
					scope === "merged" ? "stream-blocklist-merged.ndjson.gz" : "stream-blocklist.ndjson.gz",
				);
				return "Download started.";
			},
			false,
		);
	};

	const handleAddSource = async () => {
		if (!sourceURL.trim()) {
			showToast({
				type: "warning",
				title: "Enter a Source URL",
				message: "Add an http or https Stream Blocklist source URL.",
			});
			return;
		}
		await runWardenAction("add-source", "Stream Blocklist Source Added", async () => {
			const result = await apiClient.addWardenSource({
				url: sourceURL.trim(),
				name: sourceName.trim() || undefined,
				trust: "full",
				refreshHours: sourceRefreshHours,
			});
			setSourceURL("");
			setSourceName("");
			return result.message || "Source added and refresh queued.";
		});
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
		await runWardenAction("import-sources", "Stream Blocklist Sources Imported", async () => {
			const result = await apiClient.importWardenSources({
				text: sourceImportText.trim() || undefined,
				file: sourceImportFile || undefined,
				trust: "full",
				refreshHours: sourceRefreshHours,
			});
			setSourceImportText("");
			setSourceImportFile(null);
			return `${formatCount(result.added)} added, ${formatCount(result.skipped)} skipped, ${formatCount(result.invalid)} invalid.`;
		});
	};

	const handleExportSources = async () => {
		await runWardenAction(
			"export-sources",
			"Stream Blocklist Sources Export Ready",
			async () => {
				const blob = await apiClient.exportWardenSources();
				downloadBlob(blob, "stream-blocklist-sources.json");
				return "Download started.";
			},
			false,
		);
	};

	const handleUpdateSource = async (
		source: WardenSourceInfo,
		patch: { enabled?: boolean; refreshHours?: number },
	) => {
		await runWardenAction(`source-${source.id}`, "Stream Blocklist Source Updated", async () => {
			await apiClient.updateWardenSource({ id: source.id, ...patch });
			return undefined;
		});
	};

	const handleRefreshSource = async (source: WardenSourceInfo) => {
		await runWardenAction(`refresh-${source.id}`, "Stream Blocklist Source Refreshed", async () => {
			const result = await apiClient.refreshWardenSource(source.id);
			return result.message || "Refresh completed.";
		});
	};

	const handleRemoveSource = async (source: WardenSourceInfo) => {
		if (!window.confirm(`Remove ${source.name}?`)) return;
		await runWardenAction(`remove-${source.id}`, "Stream Blocklist Source Removed", async () => {
			const result = await apiClient.removeWardenSource(source.id);
			return `${formatCount(result.removed)} source removed.`;
		});
	};

	const sourceRows = wardenSources?.sources ?? [];
	const busy = wardenAction !== null;

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
							<p className="mt-2 max-w-3xl text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
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
								<p className="col-span-full mt-2 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
									Maximum number of NZBs verified per request.
								</p>
							</fieldset>
						</div>
					</div>
				)}

				{formData.enabled && (
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
								onClick={loadWardenSources}
								disabled={wardenLoading || busy}
							>
								{wardenLoading ? <LoadingSpinner size="sm" /> : <RefreshCw className="h-4 w-4" />}
								Refresh
							</button>
						</div>

						<div className="grid min-w-0 grid-cols-1 gap-6 sm:grid-cols-3">
							<div className="min-w-0">
								<div className="text-base-content/50 text-xs uppercase tracking-widest">Local</div>
								<div className="mt-1 truncate font-bold text-2xl">
									{formatCount(wardenSources?.localCount)}
								</div>
							</div>
							<div className="min-w-0">
								<div className="text-base-content/50 text-xs uppercase tracking-widest">
									Effective
								</div>
								<div className="mt-1 truncate font-bold text-2xl">
									{formatCount(wardenSources?.effectiveCount)}
								</div>
							</div>
							<div className="min-w-0">
								<div className="text-base-content/50 text-xs uppercase tracking-widest">Rows</div>
								<div className="mt-1 truncate font-bold text-2xl">
									{formatCount(wardenSources?.totalRows)}
								</div>
							</div>
						</div>

						<div className="grid min-w-0 grid-cols-1 gap-6 lg:grid-cols-2">
							<div className="min-w-0 space-y-5">
								<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
									<Database className="h-4 w-4 text-base-content/60" />
									<h5 className="font-bold text-sm">Rules</h5>
								</div>

								<div className="grid min-w-0 grid-cols-1 gap-5 sm:grid-cols-2">
									<div className="flex min-w-0 items-start justify-between gap-4">
										<div className="min-w-0">
											<div className="font-semibold text-sm">Enable Stream Blocklist</div>
											<p className="mt-1 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
												Check imported dead lists before running NNTP STAT.
											</p>
										</div>
										<input
											type="checkbox"
											className="toggle toggle-primary shrink-0"
											checked={formData.warden.enabled}
											disabled={isReadOnly}
											onChange={(e) => updateWarden({ enabled: e.target.checked })}
										/>
									</div>

									<div className="flex min-w-0 items-start justify-between gap-4">
										<div className="min-w-0">
											<div className="font-semibold text-sm">Mark Dead</div>
											<p className="mt-1 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
												Add failed STAT checks to My list.
											</p>
										</div>
										<input
											type="checkbox"
											className="toggle toggle-primary shrink-0"
											checked={formData.warden.mark_dead}
											disabled={isReadOnly}
											onChange={(e) => updateWarden({ mark_dead: e.target.checked })}
										/>
									</div>

									<div className="flex min-w-0 items-start justify-between gap-4">
										<div className="min-w-0">
											<div className="font-semibold text-sm">Backbone Scope</div>
											<p className="mt-1 max-w-full text-left text-base-content/50 text-xs leading-relaxed">
												Match dead entries against backbone domains when present.
											</p>
										</div>
										<input
											type="checkbox"
											className="toggle toggle-primary shrink-0"
											checked={formData.warden.backbone_scope}
											disabled={isReadOnly}
											onChange={(e) => updateWarden({ backbone_scope: e.target.checked })}
										/>
									</div>

									<fieldset className="fieldset min-w-0 sm:col-span-2">
										<legend className="fieldset-legend">Max Entries Per Source</legend>
										<input
											type="number"
											className="input w-full min-w-0 max-w-full"
											min={1}
											value={formData.warden.max_source_entries}
											disabled={isReadOnly}
											onChange={(e) =>
												updateWarden({
													max_source_entries: clampNum(e.target.value, 1, 100000000),
												})
											}
										/>
									</fieldset>
								</div>
							</div>

							<div className="min-w-0 space-y-5">
								<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
									<FileUp className="h-4 w-4 text-base-content/60" />
									<h5 className="font-bold text-sm">Import Stream Blocklist</h5>
								</div>

								<div className="grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2">
									<fieldset className="fieldset min-w-0 sm:col-span-2">
										<legend className="fieldset-legend">List File</legend>
										<input
											type="file"
											className="file-input w-full min-w-0 max-w-full"
											accept=".ndjson,.gz,application/gzip"
											disabled={isReadOnly || busy}
											onChange={(e) => setWardenListFile(e.target.files?.[0] ?? null)}
										/>
									</fieldset>

									<fieldset className="fieldset min-w-0">
										<legend className="fieldset-legend">Target</legend>
										<select
											className="select w-full min-w-0 max-w-full"
											value={wardenListTarget}
											disabled={isReadOnly || busy}
											onChange={(e) =>
												setWardenListTarget(e.target.value === "local" ? "local" : "separate")
											}
										>
											<option value="separate">Separate source</option>
											<option value="local">My list</option>
										</select>
									</fieldset>

									<fieldset className="fieldset min-w-0 sm:col-span-2">
										<legend className="fieldset-legend">Source Name</legend>
										<input
											type="text"
											className="input w-full min-w-0 max-w-full"
											value={wardenListName}
											placeholder="Optional"
											disabled={isReadOnly || busy || wardenListTarget === "local"}
											onChange={(e) => setWardenListName(e.target.value)}
										/>
									</fieldset>
								</div>

								<div className="flex flex-wrap gap-2">
									<button
										type="button"
										className="btn btn-primary btn-sm"
										onClick={handleImportWardenList}
										disabled={isReadOnly || busy || !wardenListFile}
									>
										{wardenAction === "import-list" ? (
											<LoadingSpinner size="sm" />
										) : (
											<Upload className="h-4 w-4" />
										)}
										Import List
									</button>
									<button
										type="button"
										className="btn btn-ghost btn-sm"
										onClick={() => handleExportWardenList("merged")}
										disabled={busy}
									>
										<Download className="h-4 w-4" />
										Export Merged
									</button>
									<button
										type="button"
										className="btn btn-ghost btn-sm"
										onClick={() => handleExportWardenList("local")}
										disabled={busy}
									>
										<Download className="h-4 w-4" />
										Export My List
									</button>
									<button
										type="button"
										className="btn btn-error btn-sm btn-outline"
										onClick={handleClearLocal}
										disabled={isReadOnly || busy}
									>
										<Trash2 className="h-4 w-4" />
										Clear My List
									</button>
								</div>
							</div>
						</div>

						<div className="grid min-w-0 grid-cols-1 gap-6 lg:grid-cols-2">
							<div className="min-w-0 space-y-5">
								<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
									<LinkIcon className="h-4 w-4 text-base-content/60" />
									<h5 className="font-bold text-sm">Add Remote Source</h5>
								</div>

								<div className="grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2">
									<fieldset className="fieldset min-w-0 sm:col-span-2">
										<legend className="fieldset-legend">URL</legend>
										<input
											type="url"
											className="input w-full min-w-0 max-w-full"
											value={sourceURL}
											placeholder="https://example.com/stream-blocklist.ndjson.gz"
											disabled={isReadOnly || busy}
											onChange={(e) => setSourceURL(e.target.value)}
										/>
									</fieldset>

									<fieldset className="fieldset min-w-0">
										<legend className="fieldset-legend">Name</legend>
										<input
											type="text"
											className="input w-full min-w-0 max-w-full"
											value={sourceName}
											placeholder="Optional"
											disabled={isReadOnly || busy}
											onChange={(e) => setSourceName(e.target.value)}
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

								<button
									type="button"
									className="btn btn-primary btn-sm"
									onClick={handleAddSource}
									disabled={isReadOnly || busy || !sourceURL.trim()}
								>
									{wardenAction === "add-source" ? (
										<LoadingSpinner size="sm" />
									) : (
										<Plus className="h-4 w-4" />
									)}
									Add Source
								</button>
							</div>

							<div className="min-w-0 space-y-5">
								<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
									<FileUp className="h-4 w-4 text-base-content/60" />
									<h5 className="font-bold text-sm">Import Source Bundle</h5>
								</div>

								<fieldset className="fieldset min-w-0">
									<legend className="fieldset-legend">Source Entries</legend>
									<textarea
										className="textarea min-h-32 w-full min-w-0 max-w-full"
										value={sourceImportText}
										placeholder="Paste URLs, one per line, or a Davex source bundle JSON."
										disabled={isReadOnly || busy}
										onChange={(e) => setSourceImportText(e.target.value)}
									/>
								</fieldset>

								<fieldset className="fieldset min-w-0">
									<legend className="fieldset-legend">Bundle File</legend>
									<input
										type="file"
										className="file-input w-full min-w-0 max-w-full"
										accept=".json,.txt,application/json,text/plain"
										disabled={isReadOnly || busy}
										onChange={(e) => setSourceImportFile(e.target.files?.[0] ?? null)}
									/>
								</fieldset>

								<div className="flex flex-wrap gap-2">
									<button
										type="button"
										className="btn btn-primary btn-sm"
										onClick={handleImportSources}
										disabled={isReadOnly || busy || (!sourceImportText.trim() && !sourceImportFile)}
									>
										{wardenAction === "import-sources" ? (
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
						</div>

						<div className="min-w-0 space-y-3">
							<div className="flex items-center gap-2 border-base-300/60 border-b pb-2">
								<Database className="h-4 w-4 text-base-content/60" />
								<h5 className="font-bold text-sm">Sources</h5>
							</div>

							<div className="overflow-x-auto rounded-xl border border-base-300/70">
								<table className="table-sm table">
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
													{wardenLoading
														? "Loading sources..."
														: "No Stream Blocklist sources yet."}
												</td>
											</tr>
										)}
										{sourceRows.map((source) => {
											const sourceBusy =
												wardenAction === `source-${source.id}` ||
												wardenAction === `refresh-${source.id}` ||
												wardenAction === `remove-${source.id}`;
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
																{wardenAction === `refresh-${source.id}` ? (
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
				)}
			</div>

			{!isReadOnly && (
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
}
