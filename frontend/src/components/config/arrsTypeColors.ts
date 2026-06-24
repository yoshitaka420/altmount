import type { ArrsType } from "../../types/config";

// Single source of truth for per-ARR-type accent colors, consumed by both
// ArrsConfigSection (ARR_TYPES) and ArrsInstanceCard (border accent). Minimal
// themes collapse primary/secondary/accent/info to one blue, so the distinct
// accents here are intentional.
export const ARR_TYPE_COLORS: Record<ArrsType, string> = {
	radarr: "bg-warning",
	sonarr: "bg-info",
	lidarr: "bg-success",
	readarr: "bg-error",
	whisparr: "bg-secondary",
	sportarr: "bg-accent",
};

// Fallback accent for any unknown/unset type.
export const ARR_TYPE_COLOR_FALLBACK = "bg-base-300";
