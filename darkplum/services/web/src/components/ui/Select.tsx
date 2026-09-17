import { forwardRef, useId } from 'react'
import styles from './Select.module.css'

interface SelectProps extends React.SelectHTMLAttributes<HTMLSelectElement> {
  label?: string
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(
  ({ label, className, children, id: idProp, ...props }, ref) => {
    const generated = useId()
    const id = idProp ?? generated

    return (
      <div className={styles.field}>
        {label && <label className={styles.label} htmlFor={id}>{label}</label>}
        <select
          ref={ref}
          id={id}
          className={`${styles.select} ${className ?? ''}`}
          {...props}
        >
          {children}
        </select>
      </div>
    )
  },
)

Select.displayName = 'Select'
