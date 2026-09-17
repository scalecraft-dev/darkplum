import { useEffect, useState } from 'react'
import styles from './Avatar.module.css'

interface AvatarProps {
  name: string
  size?: 'sm' | 'md'
  src?: string | null
}

export function Avatar({ name, size = 'sm', src }: AvatarProps) {
  const [failed, setFailed] = useState(false)

  useEffect(() => { setFailed(false) }, [src])

  const showImage = Boolean(src) && !failed

  return (
    <div className={`${styles.avatar} ${styles[size]}`} aria-hidden="true">
      {showImage ? (
        <img
          src={src as string}
          alt=""
          className={styles.image}
          referrerPolicy="no-referrer"
          onError={() => setFailed(true)}
        />
      ) : (
        name.charAt(0).toUpperCase()
      )}
    </div>
  )
}
