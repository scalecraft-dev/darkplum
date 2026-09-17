import { useId } from 'react'

export function PlumMark({ size = 32 }: { size?: number }) {
  const clipId = useId()
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 120 120"
      width={size}
      height={size}
    >
      <defs>
        <clipPath id={clipId}>
          <circle cx="60" cy="60" r="50" />
        </clipPath>
      </defs>
      <circle cx="60" cy="60" r="50" fill="var(--color-grape)" />
      <path
        d="M10 10 L110 10 L110 34 Q60 22 10 34 Z"
        fill="var(--color-moss)"
        clipPath={`url(#${clipId})`}
      />
    </svg>
  )
}
