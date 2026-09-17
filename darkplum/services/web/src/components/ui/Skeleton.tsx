import styles from './Skeleton.module.css'

interface SkeletonProps {
  width?: string
  height?: string
  borderRadius?: string
}

export function Skeleton({ width, height = '1rem', borderRadius }: SkeletonProps) {
  return (
    <span
      className={styles.skeleton}
      style={{
        width,
        height,
        borderRadius,
      }}
    />
  )
}
