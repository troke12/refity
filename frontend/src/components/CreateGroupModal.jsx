import { useState } from 'react';
import { groupsAPI } from '../services/api';
import './CreateRepositoryModal.css';

function CreateGroupModal({ show, onClose, onCreated }) {
  const [groupName, setGroupName] = useState('');
  const [error, setError] = useState('');
  const [isCreating, setIsCreating] = useState(false);
  const [success, setSuccess] = useState(false);

  const validateGroupName = (name) => {
    if (!name || name.trim().length < 2) {
      return 'Group name must be at least 2 characters long';
    }
    if (name.length > 255) {
      return 'Group name must be less than 255 characters';
    }
    if (!/^[a-z0-9._\-]+$/.test(name)) {
      return 'Group name can only contain lowercase letters, numbers, dots, hyphens, and underscores';
    }
    if (/[._\-]{2,}/.test(name)) {
      return 'Group name cannot contain consecutive special characters';
    }
    if (/^[._\-]|[._\-]$/.test(name)) {
      return 'Group name cannot start or end with special characters';
    }
    if (name.includes('/')) {
      return 'Group name cannot contain forward slashes';
    }
    return '';
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    const trimmedName = groupName.trim();
    
    const validationError = validateGroupName(trimmedName);
    if (validationError) {
      setError(validationError);
      return;
    }

    setIsCreating(true);
    setError('');
    setSuccess(false);

    try {
      await groupsAPI.create(trimmedName);
      setSuccess(true);
      setGroupName('');
      setTimeout(() => {
        onCreated();
      }, 1500);
    } catch (err) {
      setError(err.response?.data?.message || 'Failed to create group');
    } finally {
      setIsCreating(false);
    }
  };

  const handleClose = () => {
    setGroupName('');
    setError('');
    setSuccess(false);
    onClose();
  };

  if (!show) return null;

  const isValid = !validateGroupName(groupName.trim()) && groupName.trim().length >= 2;

  return (
    <div className="modal fade show d-block" style={{ backgroundColor: 'rgba(0,0,0,0.5)' }} onClick={handleClose}>
      <div className="modal-dialog modal-dialog-centered modal-lg" onClick={(e) => e.stopPropagation()}>
        <div className="modal-content">
          <div className="modal-header">
            <h5 className="modal-title">
              <i className="bi bi-plus-circle me-2"></i>Create New Group
            </h5>
            <button type="button" className="btn-close" onClick={handleClose}></button>
          </div>
          <div className="modal-body p-4">
            <form onSubmit={handleSubmit}>
              <div className="mb-3">
                <label htmlFor="group-name" className="form-label fw-bold">
                  <i className="bi bi-folder me-2"></i>Group Name
                </label>
                <input
                  type="text"
                  id="group-name"
                  className={`form-control ${error ? 'is-invalid' : isValid ? 'is-valid' : ''}`}
                  value={groupName}
                  onChange={(e) => {
                    setGroupName(e.target.value);
                    setError('');
                  }}
                  placeholder="e.g., frontend, backend, microservices"
                  disabled={isCreating}
                  autoFocus
                />
                <div className="form-text">
                  <i className="bi bi-info-circle me-1"></i>
                  Group name should contain only lowercase letters, numbers, hyphens, underscores, and dots.
                  <br />
                  <strong>Examples:</strong> <code>frontend</code>, <code>backend-services</code>, <code>microservices</code>
                </div>
                {error && (
                  <div className="invalid-feedback d-block">
                    <i className="bi bi-exclamation-circle me-1"></i>{error}
                  </div>
                )}
                {isValid && (
                  <div className="valid-feedback d-block">
                    <i className="bi bi-check-circle me-1"></i>Valid group name
                  </div>
                )}
              </div>

              {success && (
                <div className="alert alert-success d-flex align-items-center" role="alert">
                  <i className="bi bi-check-circle-fill me-2"></i>
                  Group created successfully!
                </div>
              )}
            </form>
          </div>
          <div className="modal-footer">
            <button
              type="button"
              className="btn btn-secondary"
              onClick={handleClose}
              disabled={isCreating}
            >
              <i className="bi bi-x-lg me-1"></i>Cancel
            </button>
            <button
              type="button"
              className="btn btn-primary"
              onClick={handleSubmit}
              disabled={isCreating || !isValid || !groupName.trim()}
            >
              {isCreating ? (
                <>
                  <span className="spinner-border spinner-border-sm me-2"></span>Creating...
                </>
              ) : (
                <>
                  <i className="bi bi-check-lg me-2"></i>Create Group
                </>
              )}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default CreateGroupModal;

