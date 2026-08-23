// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

/** Eigenständiges Matrix-„Digital-Rain"-Emblem (kein Nachbau der geschützten Film-Wortmarke). Grün ist Markenidentität und bleibt fix. */
export function MatrixLogo({ height = 28 }: Readonly<{ height?: number }>) {
  return (
    <svg
      role="img"
      aria-label="Matrix"
      viewBox="0 0 32 32"
      height={height}
      xmlns="http://www.w3.org/2000/svg"
      style={{ display: 'block', flex: '0 0 auto' }}
    >
      <rect
        x="2"
        y="2"
        width="28"
        height="28"
        rx="6"
        fill="#03140a"
        stroke="#00ff66"
        strokeWidth="1.6"
      />
      <g fill="#00ff66">
        <rect x="8.4" y="17" width="2.4" height="4.4" />
        <rect x="8.4" y="10.5" width="2.4" height="4.4" opacity="0.5" />
        <rect x="14.8" y="21" width="2.4" height="4.4" />
        <rect x="14.8" y="14.5" width="2.4" height="4.4" opacity="0.55" />
        <rect x="14.8" y="8" width="2.4" height="4.4" opacity="0.3" />
        <rect x="21.2" y="14" width="2.4" height="4.4" />
        <rect x="21.2" y="7.5" width="2.4" height="4.4" opacity="0.45" />
      </g>
    </svg>
  );
}
