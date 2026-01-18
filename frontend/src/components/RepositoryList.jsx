import { useState } from 'react';
import { repositoriesAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import { formatDate } from '../utils/formatDate';
import './RepositoryList.css';

function RepositoryList({ repositories, onRepositoryDeleted, onTagDeleted }) {
  const [deleting, setDeleting] = useState({});

  const handleDeleteRepository = async (repoName) => {
    if (!window.confirm(`Are you sure you want to delete repository "${repoName}" and all its tags?\n\nThis action cannot be undone.`)) {
      return;
    }

    setDeleting({ ...deleting, [repoName]: true });
    try {
      await repositoriesAPI.delete(repoName);
      onRepositoryDeleted();
    } catch (error) {
      alert('Failed to delete repository: ' + (error.response?.data?.message || 'Unknown error'));
    } finally {
      setDeleting({ ...deleting, [repoName]: false });
    }
  };

  const handleDeleteTag = async (repoName, tagName) => {
    if (!window.confirm(`Are you sure you want to delete tag "${tagName}" from repository "${repoName}"?\n\nThis action cannot be undone.`)) {
      return;
    }

    const key = `${repoName}:${tagName}`;
    setDeleting({ ...deleting, [key]: true });
    try {
      await repositoriesAPI.deleteTag(repoName, tagName);
      onTagDeleted();
    } catch (error) {
      alert('Failed to delete tag: ' + (error.response?.data?.message || 'Unknown error'));
    } finally {
      setDeleting({ ...deleting, [key]: false });
    }
  };

  return (
    <div className="repo-card fade-in">
      <div className="repo-card-header">
        <h5 className="card-title mb-0">
          <i className="bi bi-collection me-2"></i>Repositories
        </h5>
        <small className="opacity-75">List of all repositories and their tags</small>
      </div>
      <div className="card-body p-0">
        {repositories && repositories.length > 0 ? (
          <div>
            {repositories.map((repo) => (
              <div key={repo.name} className="repo-item">
                <div className="d-flex justify-content-between align-items-start">
                  <div className="flex-grow-1">
                    <div className="repo-name">
                      <i className="bi bi-folder-fill"></i>
                      {repo.name}
                    </div>
                    <div className="d-flex flex-wrap">
                      {repo.tags && repo.tags.length > 0 ? (
                        repo.tags.map((tag) => (
                          <span key={`${repo.name}:${tag.name}`} className="tag-badge">
                            <i className="bi bi-tag"></i>
                            <span>{tag.name}</span>
                            <small>({formatBytes(tag.size)})</small>
                            {tag.created_at && (
                              <small className="ms-1 text-muted" title={formatDate(tag.created_at)}>
                                <i className="bi bi-calendar3"></i>
                              </small>
                            )}
                            <button
                              onClick={() => handleDeleteTag(repo.name, tag.name)}
                              className="tag-close"
                              title="Delete tag"
                              disabled={deleting[`${repo.name}:${tag.name}`]}
                            >
                              <i className="bi bi-x"></i>
                            </button>
                          </span>
                        ))
                      ) : (
                        <span className="badge bg-secondary">
                          <i className="bi bi-inbox me-1"></i>No images yet
                        </span>
                      )}
                    </div>
                  </div>
                  <div className="ms-3">
                    <button
                      onClick={() => handleDeleteRepository(repo.name)}
                      className="btn btn-outline-danger btn-sm btn-delete"
                      disabled={deleting[repo.name]}
                    >
                      {deleting[repo.name] ? (
                        <span className="spinner-border spinner-border-sm me-1"></span>
                      ) : (
                        <i className="bi bi-trash me-1"></i>
                      )}
                      Delete
                    </button>
                  </div>
                </div>
              </div>
            ))}
          </div>
        ) : (
          <div className="empty-state">
            <i className="bi bi-folder-x"></i>
            <h5>No repositories found</h5>
            <p>Create your first repository to get started with Docker image storage.</p>
          </div>
        )}
      </div>
    </div>
  );
}

export default RepositoryList;

