import type { HealthStats } from "../../../types/api";

interface HealthStatsCardsProps {
	stats: HealthStats | undefined;
}

export function HealthStatsCards({ stats }: HealthStatsCardsProps) {
	if (!stats) {
		return null;
	}

	const healthyPercentage =
		stats.total > 0 ? ((stats.healthy / stats.total) * 100).toFixed(1) : "0.0";
	const corruptedPercentage =
		stats.total > 0 ? ((stats.corrupted / stats.total) * 100).toFixed(1) : "0.0";

	return (
		<div className="grid grid-cols-2 gap-px overflow-hidden rounded-box border border-base-content/20 bg-base-content/20 shadow-md lg:grid-cols-3 xl:grid-cols-6">
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Files Tracked</div>
				<div className="font-extrabold text-3xl text-primary">{stats.total}</div>
				<div className="text-base-content/60 text-xs">Total in database</div>
			</div>
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Healthy</div>
				<div className="font-extrabold text-3xl text-success">{stats.healthy || 0}</div>
				<div className="text-base-content/60 text-xs">{healthyPercentage}% of total</div>
			</div>
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Pending</div>
				<div className="font-extrabold text-3xl text-info">{stats.pending || 0}</div>
				<div className="text-base-content/60 text-xs">Awaiting check</div>
			</div>
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Checking</div>
				<div className="font-extrabold text-3xl text-warning">{stats.checking || 0}</div>
				<div className="text-base-content/60 text-xs">In progress</div>
			</div>
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Repairing</div>
				<div className="font-extrabold text-3xl text-secondary">{stats.repair_triggered || 0}</div>
				<div className="text-base-content/60 text-xs">Triggered</div>
			</div>
			<div className="space-y-1 bg-base-100 px-5 py-4">
				<div className="text-base-content/60 text-xs">Corrupted</div>
				<div className="font-extrabold text-3xl text-error">{stats.corrupted}</div>
				<div className="font-bold text-error text-xs">{corruptedPercentage}% - Require action</div>
			</div>
		</div>
	);
}
