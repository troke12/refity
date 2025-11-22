import { useState } from 'react';
import { repositoriesAPI } from '../services/api';
import './CreateRepositoryModal.css';

function CreateRepositoryModal({ show, onClose, onCreated }) {
  const [repoName, setRepoName] = useState('');
  const [error, setError] = useState('');
  const [isCreating, setIsCreating] = useState(false);
  const [success, setSuccess] = useState(false);

  const validateRepoName = (name) => {
    if (!name || name.trim().length < 2) {
      return 'Repository name must be at least 2 characters long';
    }
    if (name.length > 255) {
      return 'Repository name must be less than 255 characters';
    }
    if (!/^[a-z0-9._\-\/]+$/.test(name)) {
      return 'Repository name can only contain lowercase letters, numbers, dots, hyphens, underscores, and forward slashes';
    }
    if (/[._\-\/]{2,}/.test(name)) {
      return 'Repository name cannot contain consecutive special characters';
    }
    if (/^[._\-\/]|[._\-\/]$/.test(name)) {
      return 'Repository name cannot start or end with special characters';
    }
    if (name.includes('//')) {
      return 'Repository name cannot contain consecutive slashes';
    }
    return '';
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    const trimmedName = repoName.trim();
    
    const validationError = validateRepoName(trimmedName);
    if (validationError) {
      setError(validationError);
      return;
    }

    setIsCreating(true);
    setError('');
    setSuccess(false);

    try {
      await repositoriesAPI.create(trimmedName);
      setSuccess(true);
      setRepoName('');
      setTimeout(() => {
        onCreated();
      }, 1500);
    } catch (err) {
      setError(err.response?.data?.message || 'Failed to create repository');
    } finally {
      setIsCreating(false);
    }
  };

  const handleClose = () => {
    setRepoName('');
    setError('');
    setSuccess(false);
    onClose();
  };

  if (!show) return null;

  const isValid = !validateRepoName(repoName.trim()) && repoName.trim().length >= 2;

  return (
    <div className="modal fade show d-block" style={{ backgroundColor: 'rgba(0,0,0,0.5)' }} onClick={handleClose}>
      <div className="modal-dialog modal-dialog-centered modal-lg" onClick={(e) => e.stopPropagation()}>
        <div className="modal-content">
          <div className="modal-header">
            <h5 className="modal-title">
              <i className="bi bi-plus-circle me-2"></i>Create New Repository
            </h5>
            <button type="button" className="btn-close" onClick={handleClose}></button>
          </div>
          <div className="modal-body p-4">
            <form onSubmit={handleSubmit}>
              <div className="mb-3">
                <label htmlFor="repo-name" className="form-label fw-bold">
                  <i className="bi bi-folder me-2"></i>Repository Name
                </label>
                <input
                  type="text"
                  id="repo-name"
                  className={`form-control ${error ? 'is-invalid' : isValid ? 'is-valid' : ''}`}
                  value={repoName}
                  onChange={(e) => {
                    setRepoName(e.target.value);
                    setError('');
                  }}
                  placeholder="e.g., myapp, frontend-app, backend-service"
                  disabled={isCreating}
                  autoFocus
                />
                <div className="form-text">
                  <i className="bi bi-info-circle me-1"></i>
                  Repository name should contain only lowercase letters, numbers, hyphens, underscores, and forward slashes (for groups).
                  <br />
                  <strong>Examples:</strong> <code>myapp</code>, <code>frontend-app</code>, <code>group/myapp</code>
                </div>
                {error && (
                  <div className="invalid-feedback d-block">
                    <i className="bi bi-exclamation-circle me-1"></i>{error}
                  </div>
                )}
                {isValid && (
                  <div className="valid-feedback d-block">
                    <i className="bi bi-check-circle me-1"></i>Valid repository name
                  </div>
                )}
              </div>

              {success && (
                <div className="alert alert-success d-flex align-items-center" role="alert">
                  <i className="bi bi-check-circle-fill me-2"></i>
                  Repository created successfully!
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
              disabled={isCreating || !isValid || !repoName.trim()}
            >
              {isCreating ? (
                <>
                  <span className="spinner-border spinner-border-sm me-2"></span>Creating...
                </>
              ) : (
                <>
                  <i className="bi bi-check-lg me-2"></i>Create Repository
                </>
              )}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default CreateRepositoryModal;

